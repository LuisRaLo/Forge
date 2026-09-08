package yamlx

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func decode(t *testing.T, body string) (Duration, error) {
	t.Helper()
	var holder struct {
		D Duration `yaml:"d"`
	}
	err := yaml.Unmarshal([]byte(body), &holder)
	return holder.D, err
}

func TestDurationUnmarshal(t *testing.T) {
	t.Parallel()

	tests := map[string]time.Duration{
		"d: 30m":     30 * time.Minute,
		"d: 5s":      5 * time.Second,
		"d: 1h30m":   90 * time.Minute,
		"d: 90":      90 * time.Second,
		"d: \"120\"": 120 * time.Second,
		"d: 0s":      0,
	}
	for body, want := range tests {
		got, err := decode(t, body)
		if err != nil {
			t.Errorf("%q: unexpected error %v", body, err)
			continue
		}
		if got.Duration() != want {
			t.Errorf("%q = %s, want %s", body, got.Duration(), want)
		}
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"d: soon", "d: 30 minutes", "d: []"} {
		if _, err := decode(t, body); err == nil {
			t.Errorf("%q should have failed", body)
		}
	}
}

func TestDurationErrorIsActionable(t *testing.T) {
	t.Parallel()

	_, err := decode(t, "d: soon")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The message must tell the reader what to write instead.
	if !strings.Contains(err.Error(), "30m") {
		t.Errorf("error should suggest a valid form, got %q", err)
	}
}

func TestDurationRoundTrips(t *testing.T) {
	t.Parallel()

	out, err := yaml.Marshal(map[string]Duration{"d": Duration(90 * time.Second)})
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	got, err := decode(t, string(out))
	if err != nil {
		t.Fatalf("re-decode failed: %v", err)
	}
	if got.Duration() != 90*time.Second {
		t.Errorf("round trip lost value: %s", got.Duration())
	}
}
