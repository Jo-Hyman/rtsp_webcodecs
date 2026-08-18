package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// configMessage is the first text message sent over the WebSocket.
// It carries the codec name, the WebCodecs codec string and the
// decoder configuration record (AVCC / hvcC) built by the server.
type configMessage struct {
	Codec       string `json:"codec"`
	CodecString string `json:"codecString"`
	Description string `json:"description"`
	VPS         string `json:"vps,omitempty"`
	SPS         string `json:"sps"`
	PPS         string `json:"pps"`
}

// errorMessage is sent as text before closing when something goes wrong.
type errorMessage struct {
	Error string `json:"error"`
}

func b64(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// h264CodecString returns the "avc1.xxxxxx" codec string (RFC 6381).
func h264CodecString(sps []byte) string {
	if len(sps) < 4 {
		return ""
	}
	return "avc1." + hex.EncodeToString(sps[1:4])
}

// h265ProfileBlock returns the 12-byte "profile_tier_level"-derived block
// (profile_byte + 4 general_profile_compatibility_flags + 6 constraint
// bytes + general_level_idc) parsed from the SPS via mediacommon. Returns
// nil on parse failure.
func h265ProfileBlock(sps []byte) []byte {
	var s h265.SPS
	if err := s.Unmarshal(sps); err != nil {
		return nil
	}
	p := s.ProfileTierLevel

	var compat [4]byte
	for i := 0; i < 32; i++ {
		if p.GeneralProfileCompatibilityFlag[i] {
			compat[i/8] |= 1 << (7 - i%8)
		}
	}
	var constraints [6]byte
	flags := []bool{p.GeneralProgressiveSourceFlag, p.GeneralInterlacedSourceFlag,
		p.GeneralNonPackedConstraintFlag, p.GeneralFrameOnlyConstraintFlag,
		p.GeneralMax12bitConstraintFlag, p.GeneralMax10bitConstraintFlag,
		p.GeneralMax8bitConstraintFlag, p.GeneralMax422ChromeConstraintFlag,
		p.GeneralMax420ChromaConstraintFlag, p.GeneralMaxMonochromeConstraintFlag,
		p.GeneralIntraConstraintFlag, p.GeneralOnePictureOnlyConstraintFlag,
		p.GeneralLowerBitRateConstraintFlag}
	for i, f := range flags {
		if f {
			constraints[i/8] |= 1 << (7 - i%8)
		}
	}

	block := make([]byte, 0, 12)
	block = append(block, byte(p.GeneralProfileSpace<<6|p.GeneralTierFlag<<5|p.GeneralProfileIdc))
	block = append(block, compat[:]...)
	block = append(block, constraints[:]...)
	block = append(block, p.GeneralLevelIdc)
	return block
}

// h265CodecString returns the HEVC codec string per ISO/IEC 14496-15
// Annex E.3: "hvc1.<profile>.<compat>.<tier+level>". The <profile> is a
// letter (A/B/C) when general_profile_space != 0 followed by the decimal
// general_profile_idc; <compat> is the 32-bit general_profile_compatibility_flags
// as hex; <tier> is 'L' or 'H' and <level> is the decimal general_level_idc.
func h265CodecString(sps []byte) string {
	var s h265.SPS
	if err := s.Unmarshal(sps); err != nil {
		return ""
	}
	p := s.ProfileTierLevel

	profile := ""
	if p.GeneralProfileSpace == 0 {
		profile = strconv.Itoa(int(p.GeneralProfileIdc))
	} else {
		profile = string(rune('A'+p.GeneralProfileSpace-1)) + strconv.Itoa(int(p.GeneralProfileIdc))
	}

	var compat [4]byte
	for i := 0; i < 32; i++ {
		if p.GeneralProfileCompatibilityFlag[i] {
			compat[i/8] |= 1 << (7 - i%8)
		}
	}
	compatUint := uint32(compat[0])<<24 | uint32(compat[1])<<16 | uint32(compat[2])<<8 | uint32(compat[3])

	tier := "L"
	if p.GeneralTierFlag != 0 {
		tier = "H"
	}

	return fmt.Sprintf("hvc1.%s.%x.%s%d", profile, compatUint, tier, p.GeneralLevelIdc)
}

// buildAvccDescription builds an AVCDecoderConfigurationRecord.
func buildAvccDescription(sps, pps []byte) []byte {
	if len(sps) == 0 || len(pps) == 0 {
		return nil
	}
	buf := make([]byte, 11+len(sps)+len(pps))
	buf[0] = 1 // configurationVersion
	buf[1] = sps[1]
	buf[2] = sps[2]
	buf[3] = sps[3]
	buf[4] = 0xFF // reserved + lengthSizeMinusOne = 3
	buf[5] = 0xE1 // reserved + numOfSPS = 1
	binary.BigEndian.PutUint16(buf[6:8], uint16(len(sps)))
	copy(buf[8:], sps)
	buf[8+len(sps)] = 1 // numOfPPS
	binary.BigEndian.PutUint16(buf[9+len(sps):11+len(sps)], uint16(len(pps)))
	copy(buf[11+len(sps):], pps)
	return buf
}

// buildHevccDescription builds a HEVCDecoderConfigurationRecord
// (ISO/IEC 14496-15). Header is 23 bytes followed by the parameter-set arrays.
func buildHevccDescription(vps, sps, pps []byte) []byte {
	if len(vps) == 0 || len(sps) == 0 || len(pps) == 0 {
		return nil
	}
	block := h265ProfileBlock(sps)
	if block == nil {
		return nil
	}

	buf := make([]byte, 23+(5+len(vps))+(5+len(sps))+(5+len(pps)))
	buf[0] = 1 // configurationVersion
	buf[1] = block[0]
	copy(buf[2:6], block[1:5])   // general_profile_compatibility_flags
	copy(buf[6:12], block[5:11]) // general_constraint_indicator_flags
	buf[12] = block[11]          // general_level_idc
	buf[13] = 0xF0               // reserved '1111' + min_spatial_segmentation_idc high nibble
	buf[14] = 0x00               // min_spatial_segmentation_idc low byte
	buf[15] = 0xFC               // reserved '111111' + parallelismType
	buf[16] = 0xFD               // reserved '111111' + chromaFormat=1 (4:2:0)
	buf[17] = 0xF8               // reserved '11111' + bitDepthLumaMinus8=0
	buf[18] = 0xF8               // reserved '11111' + bitDepthChromaMinus8=0
	binary.BigEndian.PutUint16(buf[19:21], 0) // avgFrameRate
	buf[21] = 0x0F               // constantFrameRate=0, numTemporalLayers=1, temporalIdNested=1, lengthSizeMinusOne=3
	buf[22] = 3                  // numOfArrays (VPS, SPS, PPS)

	off := 23
	for _, arr := range []struct {
		typ  byte
		data []byte
	}{{32, vps}, {33, sps}, {34, pps}} {
		buf[off] = 0x80 | arr.typ // array_completeness + NAL_unit_type
		binary.BigEndian.PutUint16(buf[off+1:off+3], 1)
		binary.BigEndian.PutUint16(buf[off+3:off+5], uint16(len(arr.data)))
		copy(buf[off+5:], arr.data)
		off += 5 + len(arr.data)
	}
	return buf
}

// marshalLengthPrefixed encodes an access unit into the AVCC format
// (4-byte big-endian length prefix per NALU), which is what WebCodecs
// expects when a decoder configuration record is supplied.
func marshalLengthPrefixed(au [][]byte) []byte {
	size := 0
	for _, n := range au {
		size += 4 + len(n)
	}
	buf := make([]byte, 0, size)
	for _, n := range au {
		buf = append(buf, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(buf[len(buf)-4:], uint32(len(n)))
		buf = append(buf, n...)
	}
	return buf
}

func sendText(conn *websocket.Conn, mu *sync.Mutex, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		http.Error(w, "missing url query parameter", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	var sendMu sync.Mutex
	sendErr := func(msg string) {
		sendText(conn, &sendMu, errorMessage{Error: msg})
	}

	u, err := base.ParseURL(rawURL)
	if err != nil {
		sendErr("invalid RTSP URL: " + err.Error())
		return
	}

	cl := gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := cl.Start(); err != nil {
		sendErr("RTSP connection failed: " + err.Error())
		return
	}
	defer cl.Close()

	desc, _, err := cl.Describe(u)
	if err != nil {
		sendErr("RTSP DESCRIBE failed: " + err.Error())
		return
	}

	type rtpDecoder interface {
		Decode(pkt *rtp.Packet) ([][]byte, error)
	}

	var (
		medi    *description.Media
		forma   format.Format
		decoder rtpDecoder
		cfg     configMessage
		codec   string
		f264    *format.H264
		f265    *format.H265
	)

	if m := desc.FindFormat(&f265); m != nil {
		dec, err := f265.CreateDecoder()
		if err != nil {
			sendErr("H265 decoder init failed: " + err.Error())
			return
		}
		medi, forma, decoder = m, f265, dec
		codec = "h265"
		cfg = configMessage{
			Codec:       "h265",
			CodecString: h265CodecString(f265.SPS),
			Description: b64(buildHevccDescription(f265.VPS, f265.SPS, f265.PPS)),
			VPS:         b64(f265.VPS),
			SPS:         b64(f265.SPS),
			PPS:         b64(f265.PPS),
		}
	} else {
		m := desc.FindFormat(&f264)
		if m == nil {
			sendErr("no H264/H265 video stream found")
			return
		}
		dec, err := f264.CreateDecoder()
		if err != nil {
			sendErr("H264 decoder init failed: " + err.Error())
			return
		}
		medi, forma, decoder = m, f264, dec
		codec = "h264"
		cfg = configMessage{
			Codec:       "h264",
			CodecString: h264CodecString(f264.SPS),
			Description: b64(buildAvccDescription(f264.SPS, f264.PPS)),
			SPS:         b64(f264.SPS),
			PPS:         b64(f264.PPS),
		}
	}

	if err := sendText(conn, &sendMu, cfg); err != nil {
		return
	}

	if _, err := cl.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		sendErr("RTSP SETUP failed: " + err.Error())
		return
	}

	// parameter sets, initialized from SDP and updated from the stream.
	var vps, sps, pps []byte
	switch codec {
	case "h265":
		vps, sps, pps = f265.VPS, f265.SPS, f265.PPS
	case "h264":
		sps, pps = f264.SPS, f264.PPS
	}

	sendFrame := func(key bool, tsMicros int64, annexb []byte) error {
		msg := make([]byte, 9+len(annexb))
		if key {
			msg[0] = 1
		}
		binary.BigEndian.PutUint64(msg[1:9], uint64(tsMicros))
		copy(msg[9:], annexb)

		sendMu.Lock()
		defer sendMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteMessage(websocket.BinaryMessage, msg)
	}

	// derive the nominal frame duration from the SPS VUI timing info,
	// fall back to 40ms (25fps) when unavailable.
	frameDurUs := int64(40000)
	if len(sps) > 0 {
		switch codec {
		case "h265":
			var s h265.SPS
			if err := s.Unmarshal(sps); err == nil && s.VUI != nil && s.VUI.TimingInfo != nil {
				if t := s.VUI.TimingInfo; t.TimeScale > 0 {
					frameDurUs = int64(t.NumUnitsInTick) * 1000000 / int64(t.TimeScale)
				}
			}
		default:
			var s h264.SPS
			if err := s.Unmarshal(sps); err == nil && s.VUI != nil && s.VUI.TimingInfo != nil {
				if t := s.VUI.TimingInfo; t.TimeScale > 0 {
					frameDurUs = int64(t.NumUnitsInTick) * 1000000 / int64(t.TimeScale)
				}
			}
		}
	}

	// RTP timestamps may be non-monotonic when the stream uses B-frames
	// (RTP carries PTS, not DTS), so build a monotonic DTS-like timeline
	// from a simple frame counter. Good enough for video-only playback.
	frameIdx := int64(0)

	firstKeySeen := false

	cl.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		au, err := decoder.Decode(pkt)
		if err != nil {
			if !errors.Is(err, rtph264.ErrMorePacketsNeeded) &&
				!errors.Is(err, rtph265.ErrMorePacketsNeeded) &&
				!errors.Is(err, rtph264.ErrNonStartingPacketAndNoPrevious) &&
				!errors.Is(err, rtph265.ErrNonStartingPacketAndNoPrevious) {
				log.Printf("rtp decode error: %v", err)
			}
			return
		}

		var out [][]byte
		isKey := false

		switch codec {
		case "h265":
			isKey = h265.IsRandomAccess(au)
			for _, n := range au {
				if len(n) < 2 {
					continue
				}
				switch h265.NALUType((n[0] >> 1) & 0x3F) {
				case h265.NALUType_VPS_NUT:
					vps = append([]byte(nil), n...)
				case h265.NALUType_SPS_NUT:
					sps = append([]byte(nil), n...)
				case h265.NALUType_PPS_NUT:
					pps = append([]byte(nil), n...)
				}
			}
			if isKey {
				if len(vps) > 0 {
					out = append(out, vps)
				}
				if len(sps) > 0 {
					out = append(out, sps)
				}
				if len(pps) > 0 {
					out = append(out, pps)
				}
			}
			out = append(out, au...)

		default: // h264
			isKey = h264.IsRandomAccess(au)
			for _, n := range au {
				if len(n) < 2 {
					continue
				}
				switch h264.NALUType(n[0] & 0x1F) {
				case h264.NALUTypeSPS:
					sps = append([]byte(nil), n...)
				case h264.NALUTypePPS:
					pps = append([]byte(nil), n...)
				}
			}
			if isKey {
				if len(sps) > 0 {
					out = append(out, sps)
				}
				if len(pps) > 0 {
					out = append(out, pps)
				}
			}
			out = append(out, au...)
		}

		lengthPrefixed := marshalLengthPrefixed(out)

		var tsMicros int64
		if firstKeySeen || isKey {
			tsMicros = frameIdx * frameDurUs
			frameIdx++
		}
		if !isKey && !firstKeySeen {
			return // wait for the first random access unit
		}
		firstKeySeen = true

		if err := sendFrame(isKey, tsMicros, lengthPrefixed); err != nil {
			log.Printf("websocket send failed: %v", err)
		}
	})

	// detect when the browser closes the connection
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	if _, err := cl.Play(nil); err != nil {
		sendErr("RTSP PLAY failed: " + err.Error())
		return
	}

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cl.Wait()
	}()

	select {
	case <-closed:
		log.Printf("client %s disconnected", r.RemoteAddr)
	case err := <-waitErr:
		if err != nil {
			log.Printf("rtsp read ended: %v", err)
			sendErr("RTSP stream ended: " + err.Error())
		}
	}
}

func main() {
	addr := flag.String("addr", ":8088", "listen address")
	staticDir := flag.String("static", "static", "static files directory")
	flag.Parse()

	http.Handle("/", http.FileServer(http.Dir(*staticDir)))
	http.HandleFunc("/ws", handleWS)

	log.Printf("server listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
