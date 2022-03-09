// Copyright 2020, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package remux

import (
	"encoding/hex"
	"github.com/q191201771/lal/pkg/aac"
	"github.com/q191201771/lal/pkg/avc"
	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/hevc"
	"github.com/q191201771/lal/pkg/mpegts"
	"github.com/q191201771/naza/pkg/bele"
	"github.com/q191201771/naza/pkg/nazabytes"
)

var calcFragmentHeaderQueueSize = 16
var maxAudioCacheDelayByAudio uint64 = 150 * 90 // 单位（毫秒*90）
var maxAudioCacheDelayByVideo uint64 = 300 * 90 // 单位（毫秒*90）

type Rtmp2MpegtsRemuxerObserver interface {
	// OnPatPmt
	//
	// @param b const只读内存块，上层可以持有，但是不允许修改
	//
	OnPatPmt(b []byte)

	// OnTsPackets
	//
	// @param tsPackets: mpegts数据，有一个或多个188字节的ts数据组成
	//
	// @param frame: 各字段含义见 mpegts.Frame 结构体定义
	//
	OnTsPackets(tsPackets []byte, frame *mpegts.Frame, boundary bool)
}

// Rtmp2MpegtsRemuxer 输入rtmp流，输出mpegts流
//
type Rtmp2MpegtsRemuxer struct {
	UniqueKey string

	observer                Rtmp2MpegtsRemuxerObserver
	filter                  *rtmp2MpegtsFilter
	videoOut                []byte // Annexb TODO chef: 优化这块buff
	spspps                  []byte // Annexb 也可能是vps+sps+pps
	ascCtx                  *aac.AscContext
	audioCacheFrames        []byte // 缓存音频帧数据，注意，可能包含多个音频帧 TODO chef: 优化这块buff
	audioCacheFirstFramePts uint64 // audioCacheFrames中第一个音频帧的时间戳 TODO chef: rename to DTS
	audioCc                 uint8
	videoCc                 uint8

	opened bool
}

func NewRtmp2MpegtsRemuxer(observer Rtmp2MpegtsRemuxerObserver) *Rtmp2MpegtsRemuxer {
	uk := base.GenUkRtmp2MpegtsRemuxer()
	videoOut := make([]byte, 1024*1024)
	videoOut = videoOut[0:0]
	r := &Rtmp2MpegtsRemuxer{
		UniqueKey: uk,
		observer:  observer,
		videoOut:  videoOut,
	}
	r.filter = newRtmp2MpegtsFilter(calcFragmentHeaderQueueSize, r)
	return r
}

// FeedRtmpMessage
//
// @param msg: msg.Payload 调用结束后，函数内部不会持有这块内存
//
func (s *Rtmp2MpegtsRemuxer) FeedRtmpMessage(msg base.RtmpMsg) {
	s.filter.Push(msg)
}

// ---------------------------------------------------------------------------------------------------------------------

// FlushAudio
//
// 吐出音频数据的三种情况：
// 1. 收到音频或视频时，音频缓存队列已达到一定长度（内部判断）
// 2. 打开一个新的TS文件切片时
// 3. 输入流关闭时
//
func (s *Rtmp2MpegtsRemuxer) FlushAudio() {
	if s.audioCacheFrames == nil {
		return
	}

	var frame mpegts.Frame
	frame.Cc = s.audioCc
	frame.Dts = s.audioCacheFirstFramePts
	frame.Pts = s.audioCacheFirstFramePts
	frame.Key = false
	frame.Raw = s.audioCacheFrames
	frame.Pid = mpegts.PidAudio
	frame.Sid = mpegts.StreamIdAudio

	// 注意，在回调前设置为nil，因为回调中有可能再次调用FlushAudio
	s.audioCacheFrames = nil

	s.onFrame(&frame)
	// 回调结束后更新cc
	s.audioCc = frame.Cc
}

func (s *Rtmp2MpegtsRemuxer) AudioSeqHeaderCached() bool {
	return s.ascCtx != nil
}

func (s *Rtmp2MpegtsRemuxer) VideoSeqHeaderCached() bool {
	return s.spspps != nil
}

func (s *Rtmp2MpegtsRemuxer) AudioCacheEmpty() bool {
	return s.audioCacheFrames == nil
}

// ---------------------------------------------------------------------------------------------------------------------

// onPatPmt onPop
//
// 实现 rtmp2MpegtsFilterObserver
//
func (s *Rtmp2MpegtsRemuxer) onPatPmt(b []byte) {
	s.observer.OnPatPmt(b)
}

func (s *Rtmp2MpegtsRemuxer) onPop(msg base.RtmpMsg) {
	switch msg.Header.MsgTypeId {
	case base.RtmpTypeIdAudio:
		s.feedAudio(msg)
	case base.RtmpTypeIdVideo:
		s.feedVideo(msg)
	}
}

// ----- private -------------------------------------------------------------------------------------------------------

func (s *Rtmp2MpegtsRemuxer) feedVideo(msg base.RtmpMsg) {
	// 注意，有一种情况是msg.Payload为 27 02 00 00 00
	// 此时打印错误并返回也不影响
	//
	if len(msg.Payload) <= 5 {
		Log.Errorf("[%s] invalid video message length. header=%+v, payload=%s", s.UniqueKey, msg.Header, hex.Dump(msg.Payload))
		return
	}

	codecId := msg.Payload[0] & 0xF
	if codecId != base.RtmpCodecIdAvc && codecId != base.RtmpCodecIdHevc {
		return
	}

	// 将数据转换成Annexb

	// 如果是sps pps，缓存住，然后直接返回
	var err error
	if msg.IsAvcKeySeqHeader() {
		if s.spspps, err = avc.SpsPpsSeqHeader2Annexb(msg.Payload); err != nil {
			Log.Errorf("[%s] cache spspps failed. err=%+v", s.UniqueKey, err)
		}
		return
	} else if msg.IsHevcKeySeqHeader() {
		if s.spspps, err = hevc.VpsSpsPpsSeqHeader2Annexb(msg.Payload); err != nil {
			Log.Errorf("[%s] cache vpsspspps failed. err=%+v", s.UniqueKey, err)
		}
		return
	}

	cts := bele.BeUint24(msg.Payload[2:])

	audSent := false
	spsppsSent := false
	// 优化这块buffer
	out := s.videoOut[0:0]

	// msg中可能有多个NALU，逐个获取
	nals, err := avc.SplitNaluAvcc(msg.Payload[5:])
	if err != nil {
		Log.Errorf("[%s] iterate nalu failed. err=%+v, header=%+v, payload=%s", err, s.UniqueKey, msg.Header, hex.Dump(nazabytes.Prefix(msg.Payload, 32)))
		return
	}
	for _, nal := range nals {
		var nalType uint8
		switch codecId {
		case base.RtmpCodecIdAvc:
			nalType = avc.ParseNaluType(nal[0])
		case base.RtmpCodecIdHevc:
			nalType = hevc.ParseNaluType(nal[0])
		}

		//Log.Debugf("[%s] naltype=%d, len=%d(%d), cts=%d, key=%t.", s.UniqueKey, nalType, nalBytes, len(msg.Payload), cts, msg.IsVideoKeyNalu())

		// 过滤掉原流中的sps pps aud
		// sps pps前面已经缓存过了，后面有自己的写入逻辑
		// aud有自己的写入逻辑
		if (codecId == base.RtmpCodecIdAvc && (nalType == avc.NaluTypeSps || nalType == avc.NaluTypePps || nalType == avc.NaluTypeAud)) ||
			(codecId == base.RtmpCodecIdHevc && (nalType == hevc.NaluTypeVps || nalType == hevc.NaluTypeSps || nalType == hevc.NaluTypePps || nalType == hevc.NaluTypeAud)) {
			continue
		}

		// tag中的首个nalu前面写入aud
		if !audSent {
			// 注意，因为前面已经过滤了sps pps aud的信息，所以这里可以认为都是需要用aud分隔的，不需要单独判断了
			//if codecId == base.RtmpCodecIdAvc && (nalType == avc.NaluTypeSei || nalType == avc.NaluTypeIdrSlice || nalType == avc.NaluTypeSlice) {
			switch codecId {
			case base.RtmpCodecIdAvc:
				out = append(out, avc.AudNalu...)
			case base.RtmpCodecIdHevc:
				out = append(out, hevc.AudNalu...)
			}
			audSent = true
		}

		// 关键帧前追加sps pps
		if codecId == base.RtmpCodecIdAvc {
			// h264的逻辑，一个tag中，多个连续的关键帧只追加一个，不连续则每个关键帧前都追加。为什么要这样处理
			switch nalType {
			case avc.NaluTypeIdrSlice:
				if !spsppsSent {
					if out, err = s.appendSpsPps(out); err != nil {
						Log.Warnf("[%s] append spspps by not exist.", s.UniqueKey)
						return
					}
				}
				spsppsSent = true
			case avc.NaluTypeSlice:
				// 这里只有P帧，没有SEI。为什么要这样处理
				spsppsSent = false
			}
		} else {
			switch nalType {
			case hevc.NaluTypeSliceIdr, hevc.NaluTypeSliceIdrNlp, hevc.NaluTypeSliceCranut:
				if !spsppsSent {
					if out, err = s.appendSpsPps(out); err != nil {
						Log.Warnf("[%s] append spspps by not exist.", s.UniqueKey)
						return
					}
				}
				spsppsSent = true
			default:
				// 这里简化了，只要不是关键帧，就刷新标志
				spsppsSent = false
			}
		}

		// 如果写入了aud或spspps，则用start code3，否则start code4。为什么要这样处理
		// 这里不知为什么要区分写入两种类型的start code
		if len(out) == 0 {
			out = append(out, avc.NaluStartCode4...)
		} else {
			out = append(out, avc.NaluStartCode3...)
		}

		out = append(out, nal...)
	}

	dts := uint64(msg.Header.TimestampAbs) * 90

	if s.audioCacheFrames != nil && s.audioCacheFirstFramePts+maxAudioCacheDelayByVideo < dts {
		s.FlushAudio()
	}

	var frame mpegts.Frame
	frame.Cc = s.videoCc
	frame.Dts = dts
	frame.Pts = frame.Dts + uint64(cts)*90
	frame.Key = msg.IsVideoKeyNalu()
	frame.Raw = out
	frame.Pid = mpegts.PidVideo
	frame.Sid = mpegts.StreamIdVideo

	s.onFrame(&frame)
	s.videoCc = frame.Cc
}

func (s *Rtmp2MpegtsRemuxer) feedAudio(msg base.RtmpMsg) {
	if len(msg.Payload) < 3 {
		Log.Errorf("[%s] invalid audio message length. len=%d", s.UniqueKey, len(msg.Payload))
		return
	}
	if msg.Payload[0]>>4 != base.RtmpSoundFormatAac {
		return
	}

	//Log.Debugf("[%s] hls: feedAudio. dts=%d len=%d", s.UniqueKey, msg.Header.TimestampAbs, len(msg.Payload))

	if msg.Payload[1] == base.RtmpAacPacketTypeSeqHeader {
		if err := s.cacheAacSeqHeader(msg); err != nil {
			Log.Errorf("[%s] cache aac seq header failed. err=%+v", s.UniqueKey, err)
		}
		return
	}

	if !s.AudioSeqHeaderCached() {
		Log.Warnf("[%s] feed audio message but aac seq header not exist.", s.UniqueKey)
		return
	}

	pts := uint64(msg.Header.TimestampAbs) * 90

	if s.audioCacheFrames != nil && s.audioCacheFirstFramePts+maxAudioCacheDelayByAudio < pts {
		s.FlushAudio()
	}

	if s.audioCacheFrames == nil {
		s.audioCacheFirstFramePts = pts
	}

	adtsHeader := s.ascCtx.PackAdtsHeader(int(msg.Header.MsgLen - 2))
	s.audioCacheFrames = append(s.audioCacheFrames, adtsHeader...)
	s.audioCacheFrames = append(s.audioCacheFrames, msg.Payload[2:]...)
}

func (s *Rtmp2MpegtsRemuxer) cacheAacSeqHeader(msg base.RtmpMsg) error {
	var err error
	s.ascCtx, err = aac.NewAscContext(msg.Payload[2:])
	return err
}

func (s *Rtmp2MpegtsRemuxer) appendSpsPps(out []byte) ([]byte, error) {
	if s.spspps == nil {
		return out, base.ErrHls
	}

	out = append(out, s.spspps...)
	return out, nil
}

func (s *Rtmp2MpegtsRemuxer) onFrame(frame *mpegts.Frame) {
	var boundary bool

	if frame.Sid == mpegts.StreamIdAudio {
		// 为了考虑没有视频的情况也能切片，所以这里判断spspps为空时，也建议生成fragment
		boundary = !s.VideoSeqHeaderCached()
	} else {
		// 收到视频，可能触发建立fragment的条件是：
		// 关键帧数据 &&
		// (
		//  (没有收到过音频seq header) || 说明 只有视频
		//  (收到过音频seq header && fragment没有打开) || 说明 音视频都有，且都已ready
		//  (收到过音频seq header && fragment已经打开 && 音频缓存数据不为空) 说明 为什么音频缓存需不为空？
		// )
		boundary = frame.Key && (!s.AudioSeqHeaderCached() || !s.opened || !s.AudioCacheEmpty())
	}

	if boundary {
		s.opened = true
	}

	var packets []byte // TODO(chef): [refactor]
	mpegts.PackTsPacket(frame, func(packet []byte) {
		packets = append(packets, packet...)
	})

	s.observer.OnTsPackets(packets, frame, boundary)
}