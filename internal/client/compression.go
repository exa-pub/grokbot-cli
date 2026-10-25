package client

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"log"
	"net/http"

	"connectrpc.com/connect"
	"github.com/andybalholm/brotli"
)

type brotliReader struct{ *brotli.Reader }

func (*brotliReader) Close() error { return nil }

// Compression lets Connect clients accept brotli-compressed messages.
func Compression() connect.ClientOption {
	return connect.WithAcceptCompression("br",
		func() connect.Decompressor { return &brotliReader{brotli.NewReader(nil)} },
		func() connect.Compressor { return brotli.NewWriter(io.Discard) },
	)
}

// decodedBody closes both the decoder and the underlying body.
type decodedBody struct {
	io.Reader
	decoder io.Closer // nil for decoders without resources
	body    io.Closer
}

func (b *decodedBody) Close() error {
	if b.decoder != nil {
		if err := b.decoder.Close(); err != nil {
			log.Printf("close response decoder: %v", err)
		}
	}
	return b.body.Close()
}

// decodeBody transparently decompresses a response body by Content-Encoding.
func decodeBody(r *http.Response) (*http.Response, error) {
	var reader io.Reader
	var closer io.Closer
	var err error
	encoding := r.Header.Get("Content-Encoding")
	if encoding != "br" && encoding != "gzip" && encoding != "deflate" {
		return r, nil
	}
	body := bufio.NewReader(r.Body)
	// Empty bodies (204, 304, HEAD, bare errors) are passed through: they
	// carry no compressed stream, so decoding them would fail.
	if _, peekErr := body.Peek(1); errors.Is(peekErr, io.EOF) {
		encoding = ""
	}
	switch encoding {
	case "br":
		reader = brotli.NewReader(body)
	case "gzip":
		var gz *gzip.Reader
		if gz, err = gzip.NewReader(body); err == nil {
			reader, closer = gz, gz
		}
	case "deflate":
		var rc io.ReadCloser
		if rc, err = inflate(body); err == nil {
			reader, closer = rc, rc
		}
	default:
		reader = body
	}
	if err != nil {
		return nil, errors.Join(err, r.Body.Close())
	}
	r.Body = &decodedBody{Reader: reader, decoder: closer, body: r.Body}
	r.Header.Del("Content-Encoding")
	r.Header.Del("Content-Length")
	r.ContentLength = -1
	r.Uncompressed = true
	return r, nil
}

// inflate decodes "deflate" bodies, which servers send either zlib-wrapped
// (as the RFC says) or as raw DEFLATE.
func inflate(body io.Reader) (io.ReadCloser, error) {
	buf := bufio.NewReader(body)
	header, err := buf.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// A zlib header names compression method 8 (deflate) and is a big-endian
	// 16-bit multiple of 31.
	if len(header) == 2 && header[0]&0x0f == 8 && (uint16(header[0])<<8|uint16(header[1]))%31 == 0 {
		return zlib.NewReader(buf)
	}
	return flate.NewReader(buf), nil
}
