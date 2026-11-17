package hue

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

const readBufferSize = 4096

// chunk is one read of the event stream: data, or the error that ended it.
type chunk struct {
	data []byte
	err  error
}

// readChunks forwards what body yields until it fails or ctx ends. It is the
// only place that blocks on the network read, so the caller can still watch the
// idle timer and its context.
func readChunks(ctx context.Context, body io.Reader, out chan<- chunk) {
	buf := make([]byte, readBufferSize)
	for {
		n, err := body.Read(buf)
		if n > 0 && !send(ctx, out, chunk{data: append([]byte(nil), buf[:n]...)}) {
			return
		}
		if err != nil {
			send(ctx, out, chunk{err: err})
			return
		}
	}
}

func send(ctx context.Context, out chan<- chunk, c chunk) bool {
	select {
	case out <- c:
		return true
	case <-ctx.Done():
		return false
	}
}

// streamOnce follows the event stream until the connection ends, goes idle or
// ctx is cancelled. It returns only after its reader goroutine has exited.
func (h *Integration) streamOnce(ctx context.Context) error {
	resp, err := h.client.eventStream(ctx)
	if err != nil {
		return err
	}
	readCtx, stopReading := context.WithCancel(ctx)
	chunks := make(chan chunk)
	var reader sync.WaitGroup
	reader.Go(func() { readChunks(readCtx, resp.Body, chunks) })
	defer func() {
		stopReading()
		_ = resp.Body.Close() // unblocks a read in progress
		reader.Wait()
	}()

	idle := time.NewTimer(h.timing.StreamIdle)
	defer idle.Stop()
	var parser sseParser
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-idle.C:
			return errEventStreamIdle
		case next := <-chunks:
			if errors.Is(next.err, io.EOF) {
				return errEventStreamClosed
			}
			if next.err != nil {
				return next.err
			}
			h.handlePayloads(ctx, parser.push(next.data))
			// The idle time counts silence, not the time spent resyncing.
			idle.Reset(h.timing.StreamIdle)
		}
	}
}

// handlePayloads applies each payload and resyncs once per payload that asks for
// it. Sync does not read the stream, so a resync never nests.
func (h *Integration) handlePayloads(ctx context.Context, payloads []string) {
	for _, payload := range payloads {
		if !h.ApplyEventPayload(payload) {
			continue
		}
		if _, err := h.Sync(ctx); err != nil && ctx.Err() == nil {
			h.logger.Warn("hue resync failed", "error", err)
		}
	}
}

// run syncs (unless firstSync is false), then follows the event stream until ctx
// ends, reconnecting after failures with a fixed delay. Every reconnect syncs
// first.
func (h *Integration) run(ctx context.Context, firstSync bool) {
	for {
		if firstSync {
			h.syncAndLog(ctx)
		}
		firstSync = true
		if err := h.streamOnce(ctx); err != nil && ctx.Err() == nil {
			h.logger.Warn("hue: stream ended", "error", err)
		}
		if !sleep(ctx, h.timing.ReconnectDelay) {
			return
		}
	}
}

func (h *Integration) syncAndLog(ctx context.Context) {
	n, err := h.Sync(ctx)
	switch {
	case err == nil:
		h.logger.Info("hue: synced lights", "count", n)
	case ctx.Err() == nil:
		h.logger.Warn("hue: sync failed", "error", err)
	}
}

// sleep waits for d and reports false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
