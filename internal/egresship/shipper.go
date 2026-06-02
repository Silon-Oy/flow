package egresship

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"time"
)

// Sink is the upstream the shipper batches entries into. centralclient.Client
// satisfies it; tests use a fake to assert batching/rotation behaviour.
type Sink interface {
	ShipEgress(ctx context.Context, entries []Entry) error
}

// Config tunes the tail loop. Zero-valued fields fall back to sensible
// defaults — Path is the only required field.
type Config struct {
	Path          string        // squid access.log
	Sink          Sink          // upstream batcher
	BatchSize     int           // max entries flushed per request
	FlushInterval time.Duration // max time entries sit in the buffer
	PollInterval  time.Duration // poll cadence when the tail hits EOF
	Logger        *log.Logger
}

// Run tails the access log forever (until ctx is cancelled), parsing each line
// and flushing batches to the sink. It tolerates the log not yet existing
// (egress-proxy may start after the runner) and survives log rotation by
// stat'ing the path and reopening when the inode changes or the file shrinks.
//
// Invariant: parse errors are logged and skipped — a malformed line must never
// stop the shipper, otherwise a single bad write by squid would silence all
// future egress telemetry.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Path == "" {
		return errors.New("egresship: Path is required")
	}
	if cfg.Sink == nil {
		return errors.New("egresship: Sink is required")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Second
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	var (
		file   *os.File
		reader *bufio.Reader
		inode  uint64
		offset int64
	)
	defer func() {
		if file != nil {
			file.Close()
		}
	}()

	buf := make([]Entry, 0, cfg.BatchSize)
	flushTimer := time.NewTimer(cfg.FlushInterval)
	defer flushTimer.Stop()
	resetFlushTimer(flushTimer, cfg.FlushInterval)

	flush := func() {
		if len(buf) == 0 {
			return
		}
		// A 5s upper-bound on the request keeps the tail loop responsive when
		// flowd is slow; the next batch picks up after the failure.
		shipCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := cfg.Sink.ShipEgress(shipCtx, buf); err != nil {
			logger.Printf("egresship: ship batch failed (%d entries): %v", len(buf), err)
		}
		buf = buf[:0]
		resetFlushTimer(flushTimer, cfg.FlushInterval)
	}

	openOrReopen := func() {
		if file != nil {
			file.Close()
			file = nil
			reader = nil
		}
		f, err := os.Open(cfg.Path)
		if err != nil {
			if !os.IsNotExist(err) {
				logger.Printf("egresship: open %s: %v", cfg.Path, err)
			}
			return
		}
		st, err := f.Stat()
		if err != nil {
			logger.Printf("egresship: stat %s: %v", cfg.Path, err)
			f.Close()
			return
		}
		// On first open, skip historical lines — we only ship what happens
		// from now on. On reopen-after-rotate, start from the new file's head.
		var startAt int64
		if inode == 0 {
			startAt = st.Size()
		}
		if _, err := f.Seek(startAt, io.SeekStart); err != nil {
			logger.Printf("egresship: seek %s: %v", cfg.Path, err)
			f.Close()
			return
		}
		file = f
		reader = bufio.NewReader(f)
		inode = inodeOf(st)
		offset = startAt
	}

	checkRotation := func() {
		if file == nil {
			return
		}
		st, err := os.Stat(cfg.Path)
		if err != nil {
			// Disappeared (rotate-then-create); next openOrReopen retries.
			file.Close()
			file = nil
			reader = nil
			inode = 0
			offset = 0
			return
		}
		// Inode changed (rename + create) OR file truncated (copytruncate):
		// both mean a new generation, reset.
		if inodeOf(st) != inode || st.Size() < offset {
			file.Close()
			file = nil
			reader = nil
			inode = 0
			offset = 0
		}
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return nil
		case <-flushTimer.C:
			flush()
		default:
		}

		if file == nil {
			openOrReopen()
			if file == nil {
				if sleepCtx(ctx, cfg.PollInterval) {
					flush()
					return nil
				}
				continue
			}
		}

		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			offset += int64(len(line))
		}
		if err == nil {
			entry, perr := ParseLine(line)
			if errors.Is(perr, ErrSkip) {
				continue
			}
			if perr != nil {
				logger.Printf("egresship: parse: %v", perr)
				continue
			}
			buf = append(buf, entry)
			if len(buf) >= cfg.BatchSize {
				flush()
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			// Drop the partial fragment — bufio.ReadString returned it with
			// io.EOF, so the next read picks it up once squid writes the rest.
			if len(line) > 0 {
				offset -= int64(len(line))
				if _, sErr := file.Seek(offset, io.SeekStart); sErr != nil {
					logger.Printf("egresship: seek-back: %v", sErr)
				}
				reader = bufio.NewReader(file)
			}
			checkRotation()
			if sleepCtx(ctx, cfg.PollInterval) {
				flush()
				return nil
			}
			continue
		}
		// Any other read error: log, drop the handle, retry from openOrReopen.
		logger.Printf("egresship: read: %v", err)
		file.Close()
		file = nil
		reader = nil
		inode = 0
		offset = 0
		if sleepCtx(ctx, cfg.PollInterval) {
			flush()
			return nil
		}
	}
}

// sleepCtx returns true if the context was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

func resetFlushTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
