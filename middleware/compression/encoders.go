package compression

import (
	"compress/gzip"
	"io"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// encoder is what the three libraries have in common.
type encoder interface {
	io.Writer
	Flush() error
	Close() error
	Reset(dst io.Writer)
}

// encoderPool reuses the encoders of one encoding at one level, which are
// expensive to allocate.
type encoderPool struct {
	pool sync.Pool
}

// newEncoderPool returns the pool for an encoding at a level already validated by [New].
func newEncoderPool(enc Encoding, level int) *encoderPool {
	p := &encoderPool{}
	p.pool.New = func() any {
		switch enc {
		case Brotli:
			return brotli.NewWriterLevel(io.Discard, level)
		case Zstd:
			// The options are valid constants; the error only reports invalid options.
			e, _ := zstd.NewWriter(io.Discard,
				zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
				zstd.WithEncoderConcurrency(1),
				zstd.WithZeroFrames(true))

			return e
		default:
			// The level was validated by New, so there is no error.
			w, _ := gzip.NewWriterLevel(io.Discard, level)

			return w
		}
	}

	return p
}

// get returns an encoder writing to dst.
func (p *encoderPool) get(dst io.Writer) encoder {
	e, _ := p.pool.Get().(encoder)
	e.Reset(dst)

	return e
}

// put resets the encoder, so it no longer references the response writer, and
// returns it to the pool.
func (p *encoderPool) put(e encoder) {
	e.Reset(io.Discard)
	p.pool.Put(e)
}
