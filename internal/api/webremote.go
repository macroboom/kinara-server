package api

import (
	"context"
	"net/http"
	"time"

	"kinara/internal/cloud"
	"kinara/internal/store"
	"kinara/internal/transcode"
)

// Watching in a web browser away from home. The cloud's watch page
// (cloud.kinara.media/watch) signs the Kinara account in, tries this
// server's direct address and falls back to the cloud relay, then opens
// the web app here with a pass from the cloud, which /auth/cloud swaps for
// a session (cloud.go). The owner can turn it off (Settings.WebRemotePlay).
//
// Video through the relay is capped (Settings.RelayMaxKbps), since the
// relay's traffic is paid for: conversions through it are held to the cap
// (pictures above it are re-encoded), and files sent as they are through
// it are sent no faster than the cap, so a player that can't keep up steps
// down to a conversion. The web player reads the cap from /connection and
// plans for it from the start. Direct connections aren't capped.

// WebPlay says whether browsers may watch away from home (the cloud's
// watch page shows it).
func (s *Server) WebPlay() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st, err := s.Store.GetSettings(ctx)
	return err != nil || st.WebRemotePlay
}

// relayCap is the video cap in kbit/s for a request through the cloud
// relay (0: not through the relay, or no cap).
func (s *Server) relayCap(r *http.Request) int {
	if !cloud.ViaRelay(r) {
		return 0
	}
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		return store.DefaultSettings.RelayMaxKbps
	}
	return st.RelayMaxKbps
}

// capHeight is the picture height that suits a bitrate (kbit/s).
func capHeight(kbps int) int {
	if kbps <= 4500 {
		return 720
	}
	return 1080
}

// clampToCap holds a conversion request to a cap in kbit/s: the bitrate
// and height go no higher, and a copied picture above the cap is
// re-encoded. sourceBitrate is the file's in bit/s (0 when unknown).
func clampToCap(req *transcode.Request, capKbps int, sourceBitrate int64) {
	if capKbps <= 0 {
		return
	}
	if req.MaxBitrate <= 0 || req.MaxBitrate > capKbps {
		req.MaxBitrate = capKbps
	}
	if h := capHeight(capKbps); req.MaxHeight <= 0 || req.MaxHeight > h {
		req.MaxHeight = h
	}
	if req.Video == "copy" && (sourceBitrate <= 0 || sourceBitrate/1000 > int64(capKbps)) {
		req.Video = "h264"
	}
	// Lossless 7.1 alone can take several Mbit/s: under a cap it's AAC.
	if req.AudioCodec == "alac" || req.AudioCodec == "flac" {
		req.AudioCodec = "aac"
	}
}

// connectionView is how this browser or app reaches the server.
type connectionView struct {
	// Via is relay (the cloud relay), internet (another way from outside
	// the home network) or home.
	Via string `json:"via"`
	// MaxBitrate is the video cap in kbit/s on this connection (0: none).
	MaxBitrate int `json:"maxBitrate"`
}

// connection answers GET /connection: players plan for the cap from the start.
func (s *Server) connection(w http.ResponseWriter, r *http.Request, u store.User) {
	out := connectionView{Via: "home"}
	switch {
	case cloud.ViaRelay(r):
		out.Via = "relay"
	case fromInternet(r):
		out.Via = "internet"
	}
	// The relay's cap, or the owner's limit for home or away (transcoding.go).
	out.MaxBitrate = s.connectionLimit(r)
	writeJSON(w, http.StatusOK, out)
}

// throttledWriter sends no faster than a rate, after a first burst (so a
// player starts quickly and seeks don't wait).
type throttledWriter struct {
	http.ResponseWriter
	ctx     context.Context
	perSec  float64
	burst   float64
	started time.Time
	sent    float64
}

func (t *throttledWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), 64<<10)
		wrote, err := t.ResponseWriter.Write(p[:n])
		total += wrote
		t.sent += float64(wrote)
		if err != nil {
			return total, err
		}
		p = p[n:]
		if ahead := time.Duration((t.sent-t.burst)/t.perSec*float64(time.Second)) - time.Since(t.started); ahead > 0 {
			select {
			case <-t.ctx.Done():
				return total, t.ctx.Err()
			case <-time.After(ahead):
			}
		}
	}
	return total, nil
}

// relayThrottle wraps w so a file sent as it is through the relay goes no
// faster than the cap (with some headroom for bursts of a variable bitrate).
func (s *Server) relayThrottle(w http.ResponseWriter, r *http.Request) http.ResponseWriter {
	capKbps := s.relayCap(r)
	if capKbps <= 0 {
		return w
	}
	perSec := float64(capKbps) * 1000 / 8 * 1.25
	return &throttledWriter{ResponseWriter: w, ctx: r.Context(), perSec: perSec, burst: perSec * 6, started: time.Now()}
}
