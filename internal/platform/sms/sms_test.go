package sms

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestHTTPPostsTheGatewayForm(t *testing.T) {
	t.Parallel()

	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.PostForm
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	driver, err := NewHTTP(HTTPOptions{URL: srv.URL, APIKey: "k", SenderID: "Muallim", Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	if err := driver.Send(context.Background(), Message{To: "8801700000000", Text: "Fees due Friday."}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got.Get("api_key") != "k" || got.Get("senderid") != "Muallim" ||
		got.Get("number") != "8801700000000" || got.Get("message") != "Fees due Friday." {
		t.Fatalf("gateway form was %v", got)
	}
}

func TestHTTPTreatsNon2xxAsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad api key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	driver, err := NewHTTP(HTTPOptions{URL: srv.URL, APIKey: "k", Client: srv.Client()})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	if err := driver.Send(context.Background(), Message{To: "8801700000000", Text: "hi"}); err == nil {
		t.Fatal("a 401 from the gateway should be an error so River retries")
	}
}

func TestHTTPRefusesAControlCharacterInTheNumber(t *testing.T) {
	t.Parallel()

	driver, err := NewHTTP(HTTPOptions{URL: "https://gateway.test", APIKey: "k"})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	if err := driver.Send(context.Background(), Message{To: "88017\r\nBcc: x", Text: "hi"}); !errors.Is(err, ErrInvalidNumber) {
		t.Fatalf("a number with a line break was accepted: %v", err)
	}
}

func TestNewHTTPRequiresURLAndKey(t *testing.T) {
	t.Parallel()

	if _, err := NewHTTP(HTTPOptions{APIKey: "k"}); err == nil {
		t.Error("a driver with no URL should be refused")
	}
	if _, err := NewHTTP(HTTPOptions{URL: "https://gateway.test"}); err == nil {
		t.Error("a driver with no api key should be refused")
	}
}
