package scan

import "errors"

var (
	ErrNotJPEG     = errors.New("Not a valid JPEG file")
	ErrNoImageData = errors.New("No image data (SOS) found")
)

// StripJPEGMetadata drops APPn and COM segments without re-encoding. Quirks are
// kept so existing hashes stay valid: standalone markers (RSTn, EOI, 00, 01)
// are dropped, FF FF is read as a marker with a length, anything after a
// non-FF byte before SOS is dropped, segments are clamped at EOF, and
// everything after SOS is copied verbatim. Output that never reaches SOS is
// ErrNoImageData: it is bare headers, so unrelated files would share one hash.
func StripJPEGMetadata(data []byte) ([]byte, error) {
	n := len(data)
	if n < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, ErrNotJPEG
	}
	out := []byte{0xFF, 0xD8}
	pos := 2
	for pos < n-1 {
		if data[pos] != 0xFF {
			break
		}
		m := data[pos+1]
		pos += 2
		if m == 0x00 || m == 0x01 || (m >= 0xD0 && m <= 0xD9) {
			continue
		}
		if pos+2 > n {
			break
		}
		length := int(data[pos])<<8 | int(data[pos+1])
		if (m >= 0xE0 && m <= 0xEF) || m == 0xFE {
			pos += length
			continue
		}
		out = append(out, 0xFF, m)
		out = append(out, data[pos:min(pos+length, n)]...)
		pos += length
		if m == 0xDA {
			if pos < n {
				out = append(out, data[pos:]...)
			}
			return out, nil
		}
	}
	return nil, ErrNoImageData
}
