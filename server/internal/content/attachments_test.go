package content

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
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

// fetchImage reads a stored image through the public media route as an anonymous visitor.
func (e *topicEnv) fetchImage(key string) response {
	e.t.Helper()
	return e.fetchImageWithCookie(key, "")
}

// fetchImageWithCookie reads a stored image while presenting, or omitting, a session cookie.
func (e *topicEnv) fetchImageWithCookie(key, cookie string) response {
	e.t.Helper()
	request := httptest.NewRequest("GET", "/media/"+key, nil)
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	return response{status: recorder.Code, body: recorder.Body.Bytes()}
}

// decodeImage proves the served bytes are a real image and returns it for comparison.
func decodeImage(t *testing.T, data []byte) image.Image {
	t.Helper()
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("served bytes are not a decodable image: %v", err)
	}
	return decoded
}

func (e *topicEnv) attachmentRow(id uint64) (status string, deleteAfter sql.NullTime, key string) {
	e.t.Helper()
	if err := e.db.QueryRow(`SELECT status,delete_after,storage_key FROM attachments WHERE id=?`, id).
		Scan(&status, &deleteAfter, &key); err != nil {
		e.t.Fatal(err)
	}
	return status, deleteAfter, key
}

// uploadPNG uploads a PNG through the API and returns its id and storage key.
func (e *topicEnv) uploadPNG(width, height int, cookie, csrf string) (uint64, string) {
	e.t.Helper()
	res := e.upload(pngOf(e.t, width, height), "photo.png", cookie, csrf)
	expectStatus(e.t, res, 201)
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.body, &body); err != nil {
		e.t.Fatalf("cannot decode %s: %v", res.body, err)
	}
	id := parseUint(e.t, body.ID)
	_, _, key := e.attachmentRow(id)
	return id, key
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
		// A pending image is unpublished, so only its author may read it during the pending window.
		if got := env.fetchImageWithCookie(key, cookie); got.status != 200 {
			t.Fatalf("the owner must be able to read their own pending image: %d", got.status)
		}
		if got := env.fetchImage(key); got.status != 404 {
			t.Fatalf("an anonymous reader must not reach a pending image: %d", got.status)
		}
		// The stored file is a re-encode, so the bytes are not the uploaded bytes by design; what has to
		// survive is the picture itself.
		served := env.fetchImageWithCookie(key, cookie)
		decoded := decodeImage(t, served.body)
		if decoded.Bounds().Dx() != 32 || decoded.Bounds().Dy() != 24 {
			t.Fatalf("served image changed dimensions: %v", decoded.Bounds())
		}
		if !samePixels(t, decoded, decodeImage(t, pngOf(t, 32, 24))) {
			t.Fatal("served image does not show the uploaded picture")
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
			t.Fatalf("an attached image must be readable by an anonymous reader: %d", got.status)
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

// TestImagePrivacyBoundaries covers who may read a stored image. Every case is a permission boundary, so
// each one asserts the exact status rather than merely "not 200".
func TestImagePrivacyBoundaries(t *testing.T) {
	env := newTopicEnv(t)
	authorCookie, authorCSRF := env.sessionCookie(env.authorID)
	// A second real account proves that "signed in" is not the same thing as "the owner".
	otherCookie, _ := env.sessionCookie(env.newUser("second"))

	t.Run("待用图片只有作者能读，游客与其他登录用户都是404", func(t *testing.T) {
		_, key := env.uploadPNG(8, 8, authorCookie, authorCSRF)
		if got := env.fetchImageWithCookie(key, authorCookie); got.status != 200 {
			t.Fatalf("the owner must read their own pending image: %d", got.status)
		}
		if got := env.fetchImage(key); got.status != 404 {
			t.Fatalf("an anonymous reader must get 404 for a pending image: %d", got.status)
		}
		if got := env.fetchImageWithCookie(key, otherCookie); got.status != 404 {
			t.Fatalf("another signed-in user must get 404 for a pending image: %d", got.status)
		}
	})

	t.Run("过期的待用图片连作者也不能读", func(t *testing.T) {
		id, key := env.uploadPNG(8, 8, authorCookie, authorCSRF)
		if _, err := env.db.Exec(`UPDATE attachments SET delete_after=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 HOUR) WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		if got := env.fetchImageWithCookie(key, authorCookie); got.status != 404 {
			t.Fatalf("an expired pending image must be unreadable even for its owner: %d", got.status)
		}
		if got := env.fetchImage(key); got.status != 404 {
			t.Fatalf("an expired pending image must be unreadable for a guest: %d", got.status)
		}
	})

	t.Run("匿名图片在内容可见时对游客开放", func(t *testing.T) {
		uploaded := env.upload(pngOf(t, 12, 12), "public.png", authorCookie, authorCSRF)
		expectStatus(t, uploaded, 201)
		var item struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(uploaded.body, &item); err != nil {
			t.Fatal(err)
		}
		topic := env.do("POST", "/api/v1/topics",
			`{"board_id":"`+itoa(env.boardID)+`","title":"公开图片","body_md":"正文。","is_anonymous":false,"tag_ids":[],"attachment_ids":["`+item.ID+`"]}`,
			authorCookie, authorCSRF)
		expectStatus(t, topic, 201)
		_, _, key := env.attachmentRow(parseUint(t, item.ID))
		if got := env.fetchImage(key); got.status != 200 {
			t.Fatalf("an image of visible content must stay public: %d", got.status)
		}
	})

	t.Run("隐藏根主题后，回答与评论里的图片同时失效", func(t *testing.T) {
		// The moderation interface is not implemented, so this states the legal target state directly:
		// hiding a topic must hide everything shown inside it, including images carried by answers and
		// comments whose own rows stay 'visible'.
		question := env.do("POST", "/api/v1/topics",
			`{"board_id":"`+itoa(env.qaBoardID)+`","title":"隐藏祖先","body_md":"正文。","is_anonymous":false,"tag_ids":[]}`,
			authorCookie, authorCSRF)
		expectStatus(t, question, 201)
		var root struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(question.body, &root); err != nil {
			t.Fatal(err)
		}

		answerUpload := env.upload(pngOf(t, 8, 8), "answer.png", authorCookie, authorCSRF)
		expectStatus(t, answerUpload, 201)
		var answerImage struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(answerUpload.body, &answerImage); err != nil {
			t.Fatal(err)
		}
		answer := env.do("POST", "/api/v1/answers",
			`{"topic_id":"`+root.ID+`","body_md":"回答。","is_anonymous":false,"attachment_ids":["`+answerImage.ID+`"]}`,
			authorCookie, authorCSRF)
		expectStatus(t, answer, 201)
		var answerBody struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(answer.body, &answerBody); err != nil {
			t.Fatal(err)
		}

		commentUpload := env.upload(pngOf(t, 8, 8), "comment.png", authorCookie, authorCSRF)
		expectStatus(t, commentUpload, 201)
		var commentImage struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(commentUpload.body, &commentImage); err != nil {
			t.Fatal(err)
		}
		comment := env.do("POST", "/api/v1/comments",
			`{"topic_id":"`+root.ID+`","body_md":"评论。","is_anonymous":false,"attachment_ids":["`+commentImage.ID+`"]}`,
			authorCookie, authorCSRF)
		expectStatus(t, comment, 201)

		_, _, answerKey := env.attachmentRow(parseUint(t, answerImage.ID))
		_, _, commentKey := env.attachmentRow(parseUint(t, commentImage.ID))
		for name, key := range map[string]string{"回答": answerKey, "评论": commentKey} {
			if got := env.fetchImage(key); got.status != 200 {
				t.Fatalf("a visible question must serve its %s image: %d", name, got.status)
			}
		}

		if _, err := env.db.Exec(`UPDATE contents SET moderation_status='hidden' WHERE id=?`, root.ID); err != nil {
			t.Fatal(err)
		}
		// The ancestor rule matters: the answer and comment rows themselves are still 'visible', so only
		// the root check can refuse these two.
		for name, key := range map[string]string{"回答": answerKey, "评论": commentKey} {
			if got := env.fetchImage(key); got.status != 404 {
				t.Fatalf("hiding the question must hide its %s image from a guest: %d", name, got.status)
			}
			if got := env.fetchImageWithCookie(key, otherCookie); got.status != 404 {
				t.Fatalf("hiding the question must hide its %s image from another user: %d", name, got.status)
			}
		}
	})

	t.Run("隐藏回答后，其下评论的图片一并失效", func(t *testing.T) {
		// Moderation stops at the answer: the comment row and the root question both stay 'visible', so
		// resolving ancestors only down to the root topic would keep serving an image the moderator had
		// just removed along with the answer it is shown under.
		question := env.do("POST", "/api/v1/topics",
			`{"board_id":"`+itoa(env.qaBoardID)+`","title":"隐藏回答祖先","body_md":"正文。","is_anonymous":false,"tag_ids":[]}`,
			authorCookie, authorCSRF)
		expectStatus(t, question, 201)
		var root struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(question.body, &root); err != nil {
			t.Fatal(err)
		}
		created := env.do("POST", "/api/v1/answers",
			`{"topic_id":"`+root.ID+`","body_md":"回答。","is_anonymous":false}`, authorCookie, authorCSRF)
		expectStatus(t, created, 201)
		var answer struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(created.body, &answer); err != nil {
			t.Fatal(err)
		}
		uploaded := env.upload(pngOf(t, 8, 8), "under-answer.png", authorCookie, authorCSRF)
		expectStatus(t, uploaded, 201)
		var item struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(uploaded.body, &item); err != nil {
			t.Fatal(err)
		}
		comment := env.do("POST", "/api/v1/comments",
			`{"topic_id":"`+root.ID+`","answer_id":"`+answer.ID+`","body_md":"评论。","is_anonymous":false,"attachment_ids":["`+item.ID+`"]}`,
			authorCookie, authorCSRF)
		expectStatus(t, comment, 201)
		_, _, key := env.attachmentRow(parseUint(t, item.ID))
		if got := env.fetchImage(key); got.status != 200 {
			t.Fatalf("a visible answer must serve its comment image: %d", got.status)
		}
		if _, err := env.db.Exec(`UPDATE contents SET moderation_status='hidden' WHERE id=?`, answer.ID); err != nil {
			t.Fatal(err)
		}
		if got := env.fetchImage(key); got.status != 404 {
			t.Fatalf("hiding an answer must hide its comment image from a guest: %d", got.status)
		}
		if got := env.fetchImageWithCookie(key, otherCookie); got.status != 404 {
			t.Fatalf("hiding an answer must hide its comment image from another user: %d", got.status)
		}
	})

	t.Run("隐藏父评论后，回复里的图片失效而无关分支不受影响", func(t *testing.T) {
		// The second intermediate ancestor is the thread root comment: a reply is displayed inside it, so
		// hiding it has to hide the reply's image while an unrelated comment keeps serving its own.
		question := env.do("POST", "/api/v1/topics",
			`{"board_id":"`+itoa(env.qaBoardID)+`","title":"隐藏评论祖先","body_md":"正文。","is_anonymous":false,"tag_ids":[]}`,
			authorCookie, authorCSRF)
		expectStatus(t, question, 201)
		var root struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(question.body, &root); err != nil {
			t.Fatal(err)
		}
		first := env.do("POST", "/api/v1/comments",
			`{"topic_id":"`+root.ID+`","body_md":"顶层评论。","is_anonymous":false}`, authorCookie, authorCSRF)
		expectStatus(t, first, 201)
		var parentComment struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(first.body, &parentComment); err != nil {
			t.Fatal(err)
		}

		replyUpload := env.upload(pngOf(t, 8, 8), "reply.png", authorCookie, authorCSRF)
		expectStatus(t, replyUpload, 201)
		var replyImage struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(replyUpload.body, &replyImage); err != nil {
			t.Fatal(err)
		}
		reply := env.do("POST", "/api/v1/comments",
			`{"topic_id":"`+root.ID+`","reply_to_comment_id":"`+parentComment.ID+`","body_md":"回复。","is_anonymous":false,"attachment_ids":["`+replyImage.ID+`"]}`,
			authorCookie, authorCSRF)
		expectStatus(t, reply, 201)

		// An unrelated branch proves the predicate follows one thread instead of refusing every comment
		// image once anything in the topic is hidden.
		otherUpload := env.upload(pngOf(t, 8, 8), "other.png", authorCookie, authorCSRF)
		expectStatus(t, otherUpload, 201)
		var otherImage struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(otherUpload.body, &otherImage); err != nil {
			t.Fatal(err)
		}
		other := env.do("POST", "/api/v1/comments",
			`{"topic_id":"`+root.ID+`","body_md":"另一条顶层评论。","is_anonymous":false,"attachment_ids":["`+otherImage.ID+`"]}`,
			authorCookie, authorCSRF)
		expectStatus(t, other, 201)

		_, _, replyKey := env.attachmentRow(parseUint(t, replyImage.ID))
		_, _, otherKey := env.attachmentRow(parseUint(t, otherImage.ID))
		if got := env.fetchImage(replyKey); got.status != 200 {
			t.Fatalf("a visible thread must serve its reply image: %d", got.status)
		}
		if _, err := env.db.Exec(`UPDATE contents SET moderation_status='hidden' WHERE id=?`, parentComment.ID); err != nil {
			t.Fatal(err)
		}
		if got := env.fetchImage(replyKey); got.status != 404 {
			t.Fatalf("hiding the thread root comment must hide its reply image: %d", got.status)
		}
		if got := env.fetchImage(otherKey); got.status != 200 {
			t.Fatalf("an unrelated comment must keep serving its image: %d", got.status)
		}
	})

	t.Run("删除根主题后回答图片立即失效", func(t *testing.T) {
		question := env.do("POST", "/api/v1/topics",
			`{"board_id":"`+itoa(env.qaBoardID)+`","title":"删除祖先","body_md":"正文。","is_anonymous":false,"tag_ids":[]}`,
			authorCookie, authorCSRF)
		expectStatus(t, question, 201)
		var root struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(question.body, &root); err != nil {
			t.Fatal(err)
		}
		answerUpload := env.upload(pngOf(t, 8, 8), "answer.png", authorCookie, authorCSRF)
		expectStatus(t, answerUpload, 201)
		var item struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(answerUpload.body, &item); err != nil {
			t.Fatal(err)
		}
		answer := env.do("POST", "/api/v1/answers",
			`{"topic_id":"`+root.ID+`","body_md":"回答。","is_anonymous":false,"attachment_ids":["`+item.ID+`"]}`,
			authorCookie, authorCSRF)
		expectStatus(t, answer, 201)
		_, _, key := env.attachmentRow(parseUint(t, item.ID))
		del := env.do("DELETE", "/api/v1/contents/"+root.ID, "", authorCookie, authorCSRF)
		expectStatus(t, del, 200)
		if got := env.fetchImage(key); got.status != 404 {
			t.Fatalf("deleting the question must hide its answer image immediately: %d", got.status)
		}
	})

	t.Run("同一图片被多个内容引用时，任一可见即保持可读", func(t *testing.T) {
		// The rule is "at least one fully visible reference", not "every referencing row is visible", so a
		// hidden duplicate reference must not deny the public one.
		uploaded := env.upload(pngOf(t, 8, 8), "shared.png", authorCookie, authorCSRF)
		expectStatus(t, uploaded, 201)
		var item struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(uploaded.body, &item); err != nil {
			t.Fatal(err)
		}
		first := env.do("POST", "/api/v1/topics",
			`{"board_id":"`+itoa(env.boardID)+`","title":"引用一","body_md":"正文。","is_anonymous":false,"tag_ids":[],"attachment_ids":["`+item.ID+`"]}`,
			authorCookie, authorCSRF)
		expectStatus(t, first, 201)
		var firstBody struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(first.body, &firstBody); err != nil {
			t.Fatal(err)
		}
		second := env.do("POST", "/api/v1/topics",
			`{"board_id":"`+itoa(env.boardID)+`","title":"引用二","body_md":"正文。","is_anonymous":false,"tag_ids":[],"attachment_ids":["`+item.ID+`"]}`,
			authorCookie, authorCSRF)
		expectStatus(t, second, 201)
		var secondBody struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(second.body, &secondBody); err != nil {
			t.Fatal(err)
		}
		_, _, key := env.attachmentRow(parseUint(t, item.ID))
		if _, err := env.db.Exec(`UPDATE contents SET moderation_status='hidden' WHERE id=?`, firstBody.ID); err != nil {
			t.Fatal(err)
		}
		if got := env.fetchImage(key); got.status != 200 {
			t.Fatalf("one hidden reference must not hide an image another visible content still shows: %d", got.status)
		}
		if _, err := env.db.Exec(`UPDATE contents SET moderation_status='hidden' WHERE id=?`, secondBody.ID); err != nil {
			t.Fatal(err)
		}
		if got := env.fetchImage(key); got.status != 404 {
			t.Fatalf("an image no visible content references must stop being served: %d", got.status)
		}
	})

	// The rule is "at least one fully visible *reference*", and a reference is a chain rather than a row.
	// Two references to one image can share no ancestor at all, so the chains have to be judged separately:
	// a predicate that pools every step by image, as the recursive version did, lets a hidden ancestor of
	// one reference deny an image the other still shows.
	t.Run("同一图片的另一条公开引用不因某条引用的祖先被隐藏而失效", func(t *testing.T) {
		fixture := newHiddenAnswerFixture(t, env, authorCookie, authorCSRF)
		var imageID uint64
		if err := env.db.QueryRow(`SELECT id FROM attachments WHERE storage_key=?`, fixture.imageKey).Scan(&imageID); err != nil {
			t.Fatal(err)
		}
		// The same image is now shown by two content rows in different topics: the comment under the
		// fixture's answer, and a fresh public post of its own.
		public := env.do("POST", "/api/v1/topics",
			`{"board_id":"`+itoa(env.boardID)+`","title":"仍公开的图片引用","body_md":"正文。","is_anonymous":false,"tag_ids":[],"attachment_ids":["`+itoa(imageID)+`"]}`,
			authorCookie, authorCSRF)
		expectStatus(t, public, 201)
		if got := env.fetchImage(fixture.imageKey); got.status != 200 {
			t.Fatalf("an image shown by visible content must be served: %d", got.status)
		}

		// Hiding the answer removes one reference — the comment under it — and leaves the other one, and
		// the post that carries it, untouched.
		env.hideContent(t, fixture.answerID)
		if got := env.fetchImage(fixture.imageKey); got.status != 200 {
			t.Fatalf("a still public reference must keep the image served: %d", got.status)
		}

		// Once the public reference is gone too, nothing visible shows the image any more.
		var publicBody struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(public.body, &publicBody); err != nil {
			t.Fatal(err)
		}
		env.hideContent(t, publicBody.ID)
		if got := env.fetchImage(fixture.imageKey); got.status != 404 {
			t.Fatalf("an image with no reachable reference must be refused: %d", got.status)
		}
	})
}

func samePixels(t *testing.T, left, right image.Image) bool {
	t.Helper()
	if left.Bounds() != right.Bounds() {
		return false
	}
	bounds := left.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			lr, lg, lb, la := left.At(x, y).RGBA()
			rr, rg, rb, ra := right.At(x, y).RGBA()
			if lr != rr || lg != rg || lb != rb || la != ra {
				return false
			}
		}
	}
	return true
}

func parseUint(t *testing.T, raw string) uint64 {
	t.Helper()
	value, err := ParseID(raw)
	if err != nil {
		t.Fatalf("not a usable id: %q", raw)
	}
	return value
}
