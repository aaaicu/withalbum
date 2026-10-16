package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadAndDeltaSync(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.mux())
	defer server.Close()
	post := func(path, body string) (int, map[string]any) {
		resp, err := http.Post(server.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, result
	}
	status, folder := post("/api/v1/folders", `{"name":"family"}`)
	if status != 201 {
		t.Fatal(status, folder)
	}
	fid := folder["id"].(string)
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	upload := func() (int, map[string]any) {
		req, err := http.NewRequest("POST", server.URL+"/api/v1/folders/"+fid+"/assets", bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Filename", "pic.jpg")
		req.Header.Set("X-Device-ID", "iphone-A")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, out
	}
	status, first := upload()
	if status != 201 || first["result"] != "created" {
		t.Fatalf("first %d %v", status, first)
	}
	status, second := upload()
	if status != 200 || second["result"] != "already_present" {
		t.Fatalf("second %d %v", status, second)
	}
	res, err := http.Get(server.URL + "/api/v1/folders/" + fid + "/sync?cursor=0")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var data struct {
		Events     []Event `json:"events"`
		NextCursor int     `json:"next_cursor"`
	}
	if err := json.NewDecoder(res.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if len(data.Events) != 1 || data.NextCursor != 1 {
		t.Fatalf("unexpected delta %+v", data)
	}
	sha := first["asset_id"].(string)
	prev, err := http.Get(server.URL + "/api/v1/assets/" + sha + "/preview")
	if err != nil {
		t.Fatal(err)
	}
	defer prev.Body.Close()
	if prev.StatusCode != 200 {
		t.Fatal("preview missing", prev.Status)
	}
}
func TestInvalidHash(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.mux())
	defer server.Close()
	_, folder := func() (int, map[string]any) {
		r, err := http.Post(server.URL+"/api/v1/folders", "application/json", strings.NewReader(`{"name":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var x map[string]any
		if err := json.NewDecoder(r.Body).Decode(&x); err != nil {
			t.Fatal(err)
		}
		return r.StatusCode, x
	}()
	req, _ := http.NewRequest("POST", server.URL+"/api/v1/folders/"+folder["id"].(string)+"/assets", strings.NewReader("hello"))
	req.Header.Set("X-Content-SHA256", strings.Repeat("0", 64))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 422 {
		t.Fatalf("wanted 422 got %d", r.StatusCode)
	}
}
