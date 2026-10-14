package realtime

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestEncodeAndScanAudioRoundTrip(t *testing.T) {
	pcm := []byte{0, 1, 2, 3, 4, 5, 250, 251}
	spec := audioWire{extended: true, format: "pcm16", rate: 16000, ms: durationMs(pcm, "pcm16", 16000)}
	buf := encodeAudioAppend(nil, pcm, spec, [][]byte{{9, 9}})
	top := scanTop(buf)
	if !top.audioDelta() && !eq(top.typ, "input_audio_buffer.append") {
		t.Fatalf("type %s", top.typ)
	}
	if !eq(top.typ, "input_audio_buffer.append") {
		t.Fatalf("type %s", top.typ)
	}
	got, err := decodeB64(nil, top.audio)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatalf("pcm %v", got)
	}
	if top.rate != 16000 {
		t.Fatalf("rate %d", top.rate)
	}
}

func TestScanAudioDeltaNoAllocPayload(t *testing.T) {
	pcm := bytes.Repeat([]byte{0x11, 0x22}, 480)
	payload := base64.StdEncoding.EncodeToString(pcm)
	msg := []byte(`{"type":"response.output_audio.delta","delta":"` + payload + `","sample_rate_hz":24000,"server_event_seq":7}`)
	top := scanTop(msg)
	if !top.audioDelta() {
		t.Fatal("expected audio delta")
	}
	if top.seq != 7 || top.rate != 24000 {
		t.Fatalf("seq %d rate %d", top.seq, top.rate)
	}
	got, err := decodeB64(nil, top.delta)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pcm) {
		t.Fatalf("len %d", len(got))
	}
}

func TestScanLegacyAudioField(t *testing.T) {
	msg := []byte(`{"type":"response.audio.delta","audio":"AQID","format":"pcm16","sample_rate_hz":16000}`)
	top := scanTop(msg)
	if !top.audioDelta() {
		t.Fatal(string(top.typ))
	}
	if string(top.payload()) != "AQID" {
		t.Fatal(string(top.payload()))
	}
	if !eq(top.format, "pcm16") {
		t.Fatal(string(top.format))
	}
}

func TestScanSkipsNestedDelta(t *testing.T) {
	msg := []byte(`{"type":"response.done","response":{"status":"completed","delta":"nope"},"server_event_seq":3}`)
	top := scanTop(msg)
	if !eq(top.typ, "response.done") {
		t.Fatal(string(top.typ))
	}
	if top.delta != nil {
		t.Fatalf("nested delta leaked: %s", top.delta)
	}
	if top.seq != 3 {
		t.Fatal(top.seq)
	}
}

func BenchmarkEncodeAudioAppend(b *testing.B) {
	pcm := make([]byte, 960)
	spec := audioWire{extended: true, format: "pcm16", rate: 16000, ms: 20}
	b.ReportAllocs()
	b.SetBytes(int64(len(pcm)))
	for i := 0; i < b.N; i++ {
		bp := getBuf()
		*bp = encodeAudioAppend((*bp)[:0], pcm, spec, nil)
		putBuf(bp)
	}
}

func BenchmarkScanAudioDelta(b *testing.B) {
	pcm := make([]byte, 960)
	payload := base64.StdEncoding.EncodeToString(pcm)
	msg := []byte(`{"type":"response.output_audio.delta","delta":"` + payload + `","sample_rate_hz":24000}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(pcm)))
	for i := 0; i < b.N; i++ {
		top := scanTop(msg)
		if _, err := decodeB64(nil, top.delta); err != nil {
			b.Fatal(err)
		}
	}
}
