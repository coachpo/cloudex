package codexgateway

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coachpo/cloudex/apps/api/internal/codexupstream"
)

func TestResponseJSONRedactsCredentialAndPreservesSafeWire(t *testing.T) {
	grant := codexupstream.Grant{AccessToken: "fixture-credential"}
	safe := []byte(`{ "value": "ordinary\u0020text", "number": 1e400 }`)
	if got := redactResponseJSON(safe, grant); !bytes.Equal(got, safe) {
		t.Fatalf("unrelated JSON changed: %s", got)
	}
	for _, data := range []string{
		`{"value":"fixture-credential"}`,
		`{"value":"\u0066ixture-credential"}`,
	} {
		if got := redactResponseJSON([]byte(data), grant); string(got) != `{"value":"[redacted]"}` {
			t.Error("credential in a JSON string was not redacted")
		}
	}
}

func TestGatewayPreflightRedactsCredentialFromRefusals(t *testing.T) {
	for _, path := range []string{"/accounts", "/models"} {
		t.Run(path[1:], func(t *testing.T) {
			channel := codexupstream.ChannelCodex
			if path == "/models" {
				channel = codexupstream.ChannelChatGPT
			}
			const requestID = "req_fixture"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path {
					t.Errorf("upstream route = %q, want %q", r.URL.Path, path)
				}
				w.Header().Set("X-Request-Id", requestID)
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = fmt.Fprintf(w, `{"error":{"code":"rate_limited","param":"access-%s"}}`, channel)
			}))
			defer upstream.Close()
			adapter := transportAdapter(t, channel, upstream.URL, upstream.Client())
			f := newTransportFixture(t, adapter)
			_, token := f.workspace("limited", f.grant(adapter))
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+GatewayPath+"/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			res, err := f.server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("X-Request-Id") != requestID || res.Header.Get("Retry-After") != "60" ||
				!strings.Contains(string(body), `"code":"rate_limited"`) || !strings.Contains(string(body), `"param":"[redacted]"`) {
				t.Errorf("preflight refusal did not preserve public metadata and redact credentials, got HTTP %d", res.StatusCode)
			}
		})
	}
}

func TestResponsesReferencesPreservesOpaqueReferenceWithLargeNumber(t *testing.T) {
	_, refs := responsesReferences([]byte(`{"type":"reasoning","encrypted_content":"fixture-context","output":{"estimate":1e400}}`))
	want := gatewayDigest("encrypted_content", "fixture-context")
	if len(refs) != 1 || refs[0] != want {
		t.Fatalf("reference digests = %v, want [%s]", refs, want)
	}
}
