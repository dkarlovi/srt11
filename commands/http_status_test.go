package commands

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubService satisfies Service by returning whatever the test sets. Only
// Generate and Export do anything; the rest are here to satisfy the
// interface.
type stubService struct {
	generateErr error
	exportErr   error
}

func (s *stubService) Upload(name, path string) (*Session, []TagViolation, error) {
	return &Session{Name: name}, nil, nil
}
func (s *stubService) Cleanup(*Session) []CueEdit { return nil }
func (s *stubService) Generate(*Session, string) (GenerateResult, error) {
	return GenerateResult{}, s.generateErr
}
func (s *stubService) AutoFix(*Session) AutoFixResult        { return AutoFixResult{} }
func (s *stubService) Reduce(*Session) (ReduceResult, error) { return ReduceResult{}, nil }
func (s *stubService) Export(*Session) (ExportResult, error) { return ExportResult{}, s.exportErr }
func (s *stubService) UpdateCue(_ *Session, index int, _ *string, _ *float64, _ *bool) (*SessionCue, error) {
	return nil, fmt.Errorf("no cue #%d", index)
}

// stubServer wires one pre-seeded tenant around a stubService, bypassing
// localTenantFunc so no work directory or config is involved.
func stubServer(svc Service) *httpServer {
	h := newHTTPServer(nil, "", 0, 120)
	h.newTenant = func(id string) (*tenant, error) {
		return &tenant{
			id:      id,
			session: &Session{Name: "video.vtt"},
			service: svc,
			store:   &noopStore{},
		}, nil
	}
	return h
}

type noopStore struct{}

func (noopStore) Save(*Session) error     { return nil }
func (noopStore) Load() (*Session, error) { return nil, nil }
func (noopStore) Reset() error            { return nil }

func postStatus(t *testing.T, h *httpServer, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.mux().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	return w.Code, w.Body.String()
}

func TestGenerateErrorStatus(t *testing.T) {
	t.Run("the run cap is the caller's own quota, so 429", func(t *testing.T) {
		err := fmt.Errorf("%w (10 generations for this video); no audio was generated", ErrRunCapReached)
		if got := generateErrorStatus(err); got != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429", got)
		}
	})

	t.Run("an upstream failure is 502, not 429", func(t *testing.T) {
		// Reporting this as 429 would drive index.html's 429 branch to
		// claim the session is out of runs during an ElevenLabs outage.
		for _, err := range []error{
			errors.New("synthesizing cue #3 as Matko: api error - Invalid API key"),
			errors.New(`giving up after 3 attempts: unexpected HTTP status "503 Service Unavailable" returned from server`),
			errors.New("writing session vtt: permission denied"),
		} {
			if got := generateErrorStatus(err); got != http.StatusBadGateway {
				t.Errorf("status for %v = %d, want 502", err, got)
			}
		}
	})

	t.Run("handleGenerate reports the run cap as 429 with the message intact", func(t *testing.T) {
		h := stubServer(&stubService{generateErr: fmt.Errorf(
			"%w (10 generations for this video); no audio was generated, reset the session to continue", ErrRunCapReached)})
		code, body := postStatus(t, h, "/generate")
		if code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429: %s", code, body)
		}
		if !strings.Contains(body, "run cap reached (10 generations") {
			t.Errorf("body = %s, want the original run-cap wording preserved", body)
		}
	})

	t.Run("handleGenerate reports an ElevenLabs failure as 502 with the detail", func(t *testing.T) {
		h := stubServer(&stubService{generateErr: errors.New(
			"synthesizing cue #3 as Matko: api error - A voice for voice_id bogus was not found")})
		code, body := postStatus(t, h, "/generate")
		if code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502: %s", code, body)
		}
		if !strings.Contains(body, "voice_id bogus was not found") {
			t.Errorf("body = %s, want the underlying ElevenLabs message surfaced", body)
		}
	})
}

func TestExportErrorStatus(t *testing.T) {
	t.Run("unresolved overlaps are the caller's remaining work, so 409", func(t *testing.T) {
		err := fmt.Errorf("%w: same-speaker cues still overlap past tolerance", ErrExportBlocked)
		if got := exportErrorStatus(err); got != http.StatusConflict {
			t.Errorf("status = %d, want 409", got)
		}
	})

	t.Run("an upstream failure during export is 502, not 409", func(t *testing.T) {
		// Export re-runs generation, so since generateMissingVoiceLines
		// returns its errors, this path can now fail upstream too.
		err := errors.New("synthesizing cue #1 as Hana: api error - Invalid API key")
		if got := exportErrorStatus(err); got != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", got)
		}
	})

	t.Run("handleExport keeps 409 for a blocked export", func(t *testing.T) {
		h := stubServer(&stubService{exportErr: fmt.Errorf(
			"%w: same-speaker cues still overlap past tolerance; fix or re-speed the flagged lines and Generate again first", ErrExportBlocked)})
		code, body := postStatus(t, h, "/export")
		if code != http.StatusConflict {
			t.Errorf("status = %d, want 409: %s", code, body)
		}
		if !strings.Contains(body, "export blocked: same-speaker cues still overlap") {
			t.Errorf("body = %s, want the original blocked-export wording", body)
		}
	})

	t.Run("handleExport reports an upstream failure as 502", func(t *testing.T) {
		h := stubServer(&stubService{exportErr: errors.New("reading cached audio /x.mp3 duration: unexpected EOF")})
		code, body := postStatus(t, h, "/export")
		if code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502: %s", code, body)
		}
	})
}
