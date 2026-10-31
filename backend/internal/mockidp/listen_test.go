package mockidp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func TestListenAddrRequiresLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9443", "[::1]:9443", "localhost:9443"} {
		if err := RequireLoopbackListen(addr); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:9443", ":9443", "10.1.2.3:9443", "203.0.113.8:9443", "example.com:9443"} {
		if err := RequireLoopbackListen(addr); err == nil {
			t.Fatalf("%s was accepted", addr)
		}
	}
}

func TestMockTokenStaysOnLoopbackAndConfiguredAudience(t *testing.T) {
	idp, err := New("https://127.0.0.1:9443")
	if err != nil {
		t.Fatal(err)
	}
	handler := idp.Handler("odysseia-api")
	call := func(remote, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/mock/token", strings.NewReader(body))
		request.RemoteAddr = remote
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	remote := call("203.0.113.8:40000", `{"subject":"alice"}`)
	if remote.Code != http.StatusForbidden {
		t.Fatalf("remote status=%d body=%s", remote.Code, remote.Body.String())
	}
	if call("", `{"subject":"alice"}`).Code != http.StatusForbidden {
		t.Fatal("missing remote address was accepted")
	}
	otherAudience := call("127.0.0.1:9", `{"subject":"alice","audience":"other-api"}`)
	if otherAudience.Code != http.StatusBadRequest {
		t.Fatalf("audience override status=%d", otherAudience.Code)
	}
	minted := call("[::1]:9", `{"subject":"alice","alg":"RS256"}`)
	if minted.Code != http.StatusOK {
		t.Fatalf("loopback status=%d body=%s", minted.Code, minted.Body.String())
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(minted.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(payload.AccessToken, claims); err != nil {
		t.Fatal(err)
	}
	if claims["aud"] != "odysseia-api" || claims["sub"] != "alice" {
		t.Fatalf("claims = %#v", claims)
	}
}
