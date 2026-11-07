package deepai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLane4NonceRotationAndAccountIsolation(t *testing.T) {
	var seen []http.Header
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		if r.Header.Get(HeaderLane) != "4" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(r.Header.Get(HeaderInstance)) {
			t.Error("missing lane 4 or process instance")
		}
		if beacon := r.Header.Get(HeaderBeacon); !strings.Contains(beacon, ";up=") || !strings.HasSuffix(beacon, ";pi=0") || strings.Contains(beacon, "focus=") {
			t.Errorf("idle/headless beacon: %q", beacon)
		}
		if len(seen) == 1 {
			w.WriteHeader(http.StatusForbidden)
		}
		_, _ = fmt.Fprintf(w, `{"enabled":true,"balance":"0","check_in":{"nonce":"%032d","seconds":600}}`, len(seen))
	})
	client.protocol = newLaneProtocol()
	a := &Account{APIKey: "a", MachineID: "machine-a"}
	if _, err := client.Theos(context.Background(), a); err == nil {
		t.Fatal("upstream refusal was hidden")
	}
	if _, err := client.Theos(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for _, acct := range []*Account{{APIKey: "b", MachineID: "machine-a"}, {APIKey: "a", MachineID: "machine-b"}} {
		if _, err := client.Theos(context.Background(), acct); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.Theos(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"", fmt.Sprintf("%032d", 1), "", "", fmt.Sprintf("%032d", 2)} {
		if got := seen[i].Get(HeaderCheckIn); got != want {
			t.Errorf("request %d used a stale/other account nonce: got %q want %q", i, got, want)
		}
		if seen[i].Get(HeaderInstance) != seen[0].Get(HeaderInstance) {
			t.Error("instance changed between requests")
		}
	}
	otherOrigin := *client
	otherOrigin.Origin += "/other"
	if otherOrigin.checkInState(a) == client.checkInState(a) {
		t.Fatal("different API origins shared a challenge")
	}
}

func TestLane4ConcurrentCheckinsUseLatestNonce(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		want := ""
		if calls > 0 {
			want = fmt.Sprintf("%032d", calls)
		}
		if r.Header.Get(HeaderCheckIn) != want {
			t.Error("concurrent request reused an old nonce")
		}
		calls++
		_, _ = fmt.Fprintf(w, `{"enabled":true,"check_in":{"nonce":"%032d","seconds":600}}`, calls)
	})
	client.protocol = newLaneProtocol()
	account := &Account{APIKey: "concurrent"}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := client.Theos(context.Background(), account); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if calls != 8 {
		t.Fatalf("check-ins = %d", calls)
	}
	state := client.checkInState(account)
	state.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Theos(ctx, account); err != context.Canceled {
		t.Fatalf("waiting for an in-flight check-in ignored cancellation: %v", err)
	}
	<-state.gate
}

func TestLane4ModelRefreshesDueCheckinWithoutBlockingFreeModels(t *testing.T) {
	for _, refreshFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh-fails-%v", refreshFails), func(t *testing.T) {
			var requests []string
			client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.URL.Path)
				if r.URL.Path == ModelBasePath+PathTheos {
					if r.Header.Get(HeaderCheckIn) != strings.Repeat("a", 32) || !strings.HasSuffix(r.Header.Get(HeaderBeacon), ";pi=1") {
						t.Error("due check-in did not carry the nonce and actual relay activity")
					}
					if refreshFails {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					_, _ = io.WriteString(w, `{"enabled":true,"check_in":{"nonce":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","seconds":600}}`)
					return
				}
				if r.Header.Get(HeaderBeacon) != "" || r.Header.Get(HeaderCheckIn) != "" {
					t.Error("quota challenge leaked to inference")
				}
				_, _ = io.WriteString(w, `{}`)
			})
			client.protocol = newLaneProtocol()
			account := &Account{APIKey: "model-account"}
			client.checkInState(account).note([]byte(`{"check_in":{"nonce":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","seconds":600}}`), time.Now().Add(-6*time.Minute))
			for i := 0; i < 2; i++ {
				response, err := client.OpenModel(context.Background(), account, PathResponses, []byte(`{}`), "")
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
			}
			want := []string{ModelBasePath + PathTheos, ModelBasePath + PathResponses, ModelBasePath + PathResponses}
			got, _ := json.Marshal(requests)
			expected, _ := json.Marshal(want)
			if string(got) != string(expected) {
				t.Fatalf("refresh was missing or repeated per model call: %s", got)
			}
		})
	}
}

func TestLane4IgnoresMalformedChallenges(t *testing.T) {
	for _, body := range []string{`{}`, `{"check_in":{"nonce":"bad","seconds":600}}`, `{"check_in":{"nonce":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","seconds":0}}`, `{"check_in":{"nonce":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","seconds":1e30}}`} {
		state := &laneCheckIn{}
		state.note([]byte(body), time.Now())
		if state.nonce != "" {
			t.Fatal("accepted an invalid check-in")
		}
	}
}
