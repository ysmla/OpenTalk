package content

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

// pngOf renders a solid PNG of the requested size so the format and dimension checks have real bytes to
// work on rather than a crafted header.
func pngOf(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// upload posts one multipart body the way a browser form would.
func (e *topicEnv) upload(payload []byte, filename, cookie, csrf string) response {
	e.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := part.Write(payload); err != nil {
		e.t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		e.t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/api/v1/attachments", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", testOrigin)
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	return response{status: recorder.Code, body: recorder.Body.Bytes()}
}

// fetchImage reads a stored image through the public media route.
func (e *topicEnv) fetchImage(key string) response {
	e.t.Helper()
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/media/"+key, nil))
	return response{status: recorder.Code, body: recorder.Body.Bytes()}
}

func (e *topicEnv) attachmentRow(id uint64) (status string, deleteAfter sql.NullTime, key string) {
	e.t.Helper()
	if err := e.db.QueryRow(`SELECT status,delete_after,storage_key FROM attachments WHERE id=?`, id).
		Scan(&status, &deleteAfter, &key); err != nil {
		e.t.Fatal(err)
	}
	return status, deleteAfter, key
}

func TestImageUploadAndServing(t *testing.T) {
	env := newTopicEnv(t)
	cookie, csrf := env.sessionCookie(env.authorID)

	t.Run("上传PNG后写入待用行并可读取", func(t *testing.T) {
		res := env.upload(pngOf(t, 32, 24), "photo.png", cookie, csrf)
		expectStatus(t, res, 201)
		var body struct {
			ID       string `json:"id"`
			URL      string `json:"url"`
			MIMEType string `json:"mime_type"`
			ByteSize int64  `json:"byte_size"`
		}
		if err := json.Unmarshal(res.body, &body); err != nil {
			t.Fatalf("cannot decode %s: %v", res.body, err)
		}
		// The MIME type comes from the decoded bytes, not from the filename or a declared header.
		if body.MIMEType != "image/png" || !strings.HasPrefix(body.URL, "/media/") {
			t.Fatalf("unexpected attachment: %s", res.body)
		}
		status, deleteAfter, key := env.attachmentRow(parseUint(t, body.ID))
		if status != "pending" || !deleteAfter.Valid {
			t.Fatalf("a fresh upload must be pending with a deadline: %s %v", status, deleteAfter)
		}
		if got := env.fetchImage(key); got.status != 200 {
			t.Fatalf("a pending image must be readable during its window: %d", got.status)
		}
		// The served bytes must be the file that was stored, not a re-encode.
		if !bytes.Equal(env.fetchImage(key).body, pngOf(t, 32, 24)) {
			t.Fatal("served image differs from the uploaded bytes")
		}
	})

	t.Run("关联到内容后提升为active并清除清理期限", func(t *testing.T) {
		upload := env.upload(pngOf(t, 16, 16), "inline.png", cookie, csrf)
		expectStatus(t, upload, 201)
		var uploaded struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}
		if err := json.Unmarshal(upload.body, &uploaded); err != nil {
			t.Fatal(err)
		}
		id := parseUint(t, uploaded.ID)
		payload := `{"board_id":"` + itoa(env.boardID) + `","title":"带图的帖子","body_md":"正文。","is_anonymous":false,"tag_ids":[],"attachment_ids":["` + uploaded.ID + `"]}`
		topicRes := env.do("POST", "/api/v1/topics", payload, cookie, csrf)
		expectStatus(t, topicRes, 201)
		status, deleteAfter, key := env.attachmentRow(id)
		// This is the fix for a real bug: without promotion the cleanup job would delete the images of a
		// published topic once the pending window expired.
		if status != "active" || deleteAfter.Valid {
			t.Fatalf("attaching must promote the image: status=%s delete_after=%v", status, deleteAfter)
		}
		if got := env.fetchImage(key); got.status != 200 {
			t.Fatalf("an attached image must be readable: %d", got.status)
		}
		// Deleting the content must hide its images at once, without waiting for the cleanup job.
		var topicID string
		var decoded struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(topicRes.body, &decoded); err != nil {
			t.Fatal(err)
		}
		topicID = decoded.ID
		del := env.do("DELETE", "/api/v1/contents/"+topicID, "", cookie, csrf)
		expectStatus(t, del, 200)
		if got := env.fetchImage(key); got.status != 404 {
			t.Fatalf("an image of deleted content must stop being served immediately: %d", got.status)
		}
	})

	t.Run("拒绝非图片与超限图片", func(t *testing.T) {
		res := env.upload([]byte("this is not an image"), "notes.txt", cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "UNSUPPORTED_IMAGE")
		// A dimension over the configured limit is refused even though the bytes are a valid PNG.
		res = env.upload(pngOf(t, 4100, 8), "wide.png", cookie, csrf)
		expectStatus(t, res, 422)
		expectCode(t, res, "IMAGE_TOO_LARGE")
	})

	t.Run("未登录与缺少CSRF被拒", func(t *testing.T) {
		res := env.upload(pngOf(t, 8, 8), "a.png", "", "")
		expectStatus(t, res, 401)
		res = env.upload(pngOf(t, 8, 8), "a.png", cookie, "")
		expectStatus(t, res, 403)
		expectCode(t, res, "CSRF_REJECTED")
	})

	t.Run("未知与穿越型key都返回404", func(t *testing.T) {
		if got := env.fetchImage(strings.Repeat("a", 32)); got.status != 404 {
			t.Fatalf("an unknown key must be 404, got %d", got.status)
		}
		// A key that is not exactly the generated shape must not reach the filesystem at all.
		for _, key := range []string{"..%2fsecret", "short", strings.Repeat("z", 32), strings.Repeat("a", 33)} {
			if got := env.fetchImage(key); got.status != 404 {
				t.Fatalf("key %q must be refused with 404, got %d", key, got.status)
			}
		}
	})
}

func parseUint(t *testing.T, raw string) uint64 {
	t.Helper()
	value, err := ParseID(raw)
	if err != nil {
		t.Fatalf("not a usable id: %q", raw)
	}
	return value
}
