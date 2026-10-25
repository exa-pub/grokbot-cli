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
	switch r.Header.Get("Content-Encoding") {
	case "br":
		reader = brotli.NewReader(r.Body)
	case "gzip":
		var gz *gzip.Reader
		if gz, err = gzip.NewReader(r.Body); err == nil {
			reader, closer = gz, gz
		}
	case "deflate":
		var rc io.ReadCloser
		if rc, err = inflate(r.Body); err == nil {
			reader, closer = rc, rc
		}
	default:
		return r, nil
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
	// A zlib header is a big-endian 16-bit multiple of 31.
	if len(header) == 2 && (uint16(header[0])<<8|uint16(header[1]))%31 == 0 {
		return zlib.NewReader(buf)
	}
	return flate.NewReader(buf), nil
}
