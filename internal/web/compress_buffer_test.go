package web

import (
	"bytes"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// The compressor holds bytes back only until it knows whether the body clears the
// 1 KiB threshold. It used to hold back the whole first Write instead: the JSON
// endpoints marshal into one buffer and hand it over in a single call, so a
// multi-megabyte response was duplicated in memory to answer a question the first
// kilobyte already settles.
func TestSniffBufferNeverGrowsPastTheThreshold(t *testing.T) {
	const bodySize = 8 << 20 // 8 MiB in ONE Write - the shape writeJSON produces

	// Measured by allocation, not by reading g.sniff: decide() nils the buffer on
	// its way out, so by the time a handler could look, the copy it made has
	// already been dropped. The cost is real but transient, which is exactly what
	// TotalAlloc records and an after-the-fact length check cannot.
	body := bytes.Repeat([]byte("a"), bodySize)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
	handler := compressResponses(h)

	// One warm-up so the gzip pool and buffers are already allocated and only the
	// per-request behaviour is being weighed.
	gzGet(t, handler, "/api/status", "gzip")

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	rr := gzGet(t, handler, "/api/status", "gzip")
	runtime.ReadMemStats(&after)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	grew := after.TotalAlloc - before.TotalAlloc
	// Holding the whole write costs at least one more copy of the body; the
	// threshold decision needs 1 KiB. Half the body is a wide margin either side.
	if grew > bodySize/2 {
		t.Errorf("serving an %d-byte single-write body allocated %d bytes; the compressor is "+
			"copying the whole body to decide whether it clears a %d-byte threshold",
			bodySize, grew, compressMinBytes)
	}
}

// ...and the body must still arrive intact and compressed.
func TestALargeSingleWriteStillCompressesCorrectly(t *testing.T) {
	body := strings.Repeat("pingularity ", 200_000) // ~2.4 MiB, highly compressible
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})
	rr := gzGet(t, compressResponses(h), "/api/status", "gzip")
	if ce := rr.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	got := string(gunzip(t, rr.Body.Bytes()))
	if got != body {
		t.Fatalf("round-tripped body differs: got %d bytes, want %d", len(got), len(body))
	}
}

// A body that arrives in many small writes and never reaches the threshold must
// still be sent uncompressed and whole - the path where the sniff buffer IS the
// response.
func TestManySmallWritesUnderTheThresholdStayPlain(t *testing.T) {
	const chunks, chunk = 8, 100 // 800 bytes total, under 1 KiB
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i < chunks; i++ {
			w.Write(bytes.Repeat([]byte("b"), chunk))
		}
	})
	rr := gzGet(t, compressResponses(h), "/api/status", "gzip")
	if ce := rr.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none: %d bytes is under the %d threshold",
			ce, chunks*chunk, compressMinBytes)
	}
	if n := rr.Body.Len(); n != chunks*chunk {
		t.Errorf("body = %d bytes, want %d", n, chunks*chunk)
	}
}

// The boundary itself: writes that straddle the threshold must not lose or
// duplicate the bytes at the seam, which is where the new split happens.
func TestWritesStraddlingTheThresholdKeepEveryByte(t *testing.T) {
	for _, first := range []int{1, compressMinBytes - 1, compressMinBytes, compressMinBytes + 1} {
		var want bytes.Buffer
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			a := bytes.Repeat([]byte("x"), first)
			b := bytes.Repeat([]byte("y"), 4096)
			want.Write(a)
			want.Write(b)
			w.Write(a)
			w.Write(b)
		})
		rr := gzGet(t, compressResponses(h), "/api/status", "gzip")
		var got []byte
		if rr.Header().Get("Content-Encoding") == "gzip" {
			got = gunzip(t, rr.Body.Bytes())
		} else {
			got = rr.Body.Bytes()
		}
		if !bytes.Equal(got, want.Bytes()) {
			t.Errorf("first write %d: body round-tripped as %d bytes, want %d", first, len(got), want.Len())
		}
		want.Reset()
	}
}
