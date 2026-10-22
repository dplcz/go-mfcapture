package oggopus

import (
	"bytes"
	"encoding/binary"
	"testing"
)

type parsedPage struct {
	flags   byte
	granule int64
	serial  uint32
	seq     uint32
	packets [][]byte
}

// parseOggPages 按规范拆页、拆包，并重新校验 CRC。任何结构错误都直接失败，
// 这样这个测试同时是 writer 的格式守门员。
func parseOggPages(t *testing.T, b []byte) []parsedPage {
	t.Helper()
	var pages []parsedPage
	for off := 0; off < len(b); {
		if len(b)-off < 27 {
			t.Fatalf("truncated page header at offset %d", off)
		}
		hdr := b[off : off+27]
		if string(hdr[0:4]) != "OggS" {
			t.Fatalf("bad capture pattern at offset %d: %q", off, hdr[0:4])
		}
		if hdr[4] != 0 {
			t.Fatalf("page version = %d, want 0", hdr[4])
		}
		segments := int(hdr[26])
		if len(b)-off-27 < segments {
			t.Fatalf("truncated segment table at offset %d", off)
		}
		lacing := b[off+27 : off+27+segments]
		payloadLen := 0
		for _, v := range lacing {
			payloadLen += int(v)
		}
		pageLen := 27 + segments + payloadLen
		if len(b)-off < pageLen {
			t.Fatalf("truncated page payload at offset %d", off)
		}
		page := b[off : off+pageLen]

		want := binary.LittleEndian.Uint32(hdr[22:26])
		zeroed := append([]byte(nil), page...)
		binary.LittleEndian.PutUint32(zeroed[22:26], 0)
		if got := crc(zeroed); got != want {
			t.Fatalf("page at offset %d: CRC 0x%08X, want 0x%08X", off, got, want)
		}

		p := parsedPage{
			flags:   hdr[5],
			granule: int64(binary.LittleEndian.Uint64(hdr[6:14])),
			serial:  binary.LittleEndian.Uint32(hdr[14:18]),
			seq:     binary.LittleEndian.Uint32(hdr[18:22]),
		}
		payload := page[27+segments:]
		start := 0
		for _, v := range lacing {
			start += int(v)
			if v < 255 {
				p.packets = append(p.packets, append([]byte(nil), payload[0:start]...))
				payload = payload[start:]
				start = 0
			}
		}
		pages = append(pages, p)
		off += pageLen
	}
	return pages
}

func TestOggOpusWriterLayout(t *testing.T) {
	var buf bytes.Buffer
	const preSkip = 312
	w, err := NewWriter(&buf, 2, preSkip, 0x1234)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// 255 字节与 300 字节覆盖 lacing 的边界；3 字节覆盖短包。
	packets := [][]byte{
		{1, 2, 3},
		bytes.Repeat([]byte{0xAA}, 255),
		bytes.Repeat([]byte{0xBB}, 300),
	}
	for _, p := range packets {
		if err := w.WritePacket(p, 960); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	pages := parseOggPages(t, buf.Bytes())
	if len(pages) != 2+len(packets) {
		t.Fatalf("got %d pages, want %d", len(pages), 2+len(packets))
	}

	for i, p := range pages {
		if p.serial != 0x1234 {
			t.Errorf("page %d serial = %#x, want 0x1234", i, p.serial)
		}
		if p.seq != uint32(i) {
			t.Errorf("page %d sequence = %d", i, p.seq)
		}
	}
	if pages[0].flags&FlagBOS == 0 {
		t.Error("first page is not marked BOS")
	}
	if pages[0].granule != 0 || pages[1].granule != 0 {
		t.Errorf("header granule = %d/%d, want 0/0", pages[0].granule, pages[1].granule)
	}
	if got := pages[0].packets; len(got) != 1 || len(got[0]) != 19 || string(got[0][0:8]) != "OpusHead" {
		t.Fatalf("first packet = %q, want OpusHead", got)
	}
	head := pages[0].packets[0]
	if head[8] != 1 || head[9] != 2 {
		t.Errorf("OpusHead version/channels = %d/%d, want 1/2", head[8], head[9])
	}
	if got := binary.LittleEndian.Uint16(head[10:]); got != preSkip {
		t.Errorf("OpusHead pre-skip = %d, want %d", got, preSkip)
	}
	if got := binary.LittleEndian.Uint32(head[12:]); got != 48000 {
		t.Errorf("OpusHead input rate = %d, want 48000", got)
	}
	if len(pages[1].packets) != 1 || string(pages[1].packets[0][0:8]) != "OpusTags" {
		t.Fatalf("second packet = %q, want OpusTags", pages[1].packets)
	}

	for i, want := range packets {
		p := pages[2+i]
		if len(p.packets) != 1 || !bytes.Equal(p.packets[0], want) {
			t.Errorf("audio page %d payload mismatch (%d bytes, want %d)", i, len(p.packets), len(want))
		}
		if granule, wantGranule := p.granule, int64(960*(i+1)); granule != wantGranule {
			t.Errorf("audio page %d granule = %d, want %d", i, granule, wantGranule)
		}
		if wantEOS := i == len(packets)-1; (p.flags&FlagEOS != 0) != wantEOS {
			t.Errorf("audio page %d EOS = %v, want %v", i, p.flags&FlagEOS != 0, wantEOS)
		}
	}
}

func TestOggOpusWriterEmptyStream(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, 1, 312, 7)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	pages := parseOggPages(t, buf.Bytes())
	if len(pages) != 3 {
		t.Fatalf("got %d pages, want head + tags + empty EOS", len(pages))
	}
	last := pages[2]
	if last.flags&FlagEOS == 0 {
		t.Error("final page is not marked EOS")
	}
	if last.granule != -1 {
		t.Errorf("empty EOS granule = %d, want -1", last.granule)
	}
	if len(last.packets) != 0 {
		t.Errorf("empty EOS page has %d packets", len(last.packets))
	}
}

func TestLacing(t *testing.T) {
	cases := []struct {
		n    int
		want []byte
	}{
		{0, nil},
		{1, []byte{1}},
		{254, []byte{254}},
		{255, []byte{255, 0}},
		{300, []byte{255, 45}},
		{510, []byte{255, 255, 0}},
	}
	for _, c := range cases {
		if got := lacing(c.n); !bytes.Equal(got, c.want) {
			t.Errorf("lacing(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}
