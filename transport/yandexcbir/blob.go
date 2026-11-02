package yandexcbir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
)

const (
	// uploadURL answers with the cbirId of the stored image.
	uploadURL = "https://yandex.ru/images/search?rpt=imageview&format=json&request=" +
		"%7B%22blocks%22%3A%5B%7B%22block%22%3A%22b-page_type_search-by-image__link%22%7D%5D%7D"
	// cbirPrefix serves the stored image back, byte for byte.
	cbirPrefix = "https://avatars.mds.yandex.net/get-images-cbir/"
)

// upload stores payload as an image and returns its cbirId.
func upload(c *http.Client, payload []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="upfile"; filename="image.png"`}
	h["Content-Type"] = []string{"image/png"}
	fw, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	// One flag byte says how the payload is framed: 0 raw, 1 deflated.
	framed := append([]byte{0}, payload...)
	if z := Deflate(payload); len(z) < len(payload) {
		framed = append([]byte{1}, z...)
	}
	if _, err := fw.Write(WrapPNG(framed)); err != nil {
		return "", err
	}
	_ = mw.Close()

	req, err := http.NewRequest("POST", uploadURL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out struct {
		Blocks []struct {
			Params struct {
				CbirID string `json:"cbirId"`
			} `json:"params"`
		} `json:"blocks"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Blocks) == 0 {
		return "", fmt.Errorf("yandexcbir: upload answered %s: %.400s", res.Status, raw)
	}
	id := out.Blocks[0].Params.CbirID
	if id == "" {
		return "", fmt.Errorf("yandexcbir: upload gave no cbirId: %.400s", raw)
	}
	return id, nil
}

// download fetches the payload of the image with this cbirId.
func download(c *http.Client, id string) ([]byte, error) {
	req, err := http.NewRequest("GET", cbirPrefix+id+"/orig", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("yandexcbir: download %s: %s", id, res.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	payload, err := UnwrapPNG(raw)
	if err != nil {
		return nil, err
	}
	// A chunk written deflated says so with its first byte; see packFrame.
	if len(payload) > 0 && payload[0] == 1 {
		return Inflate(payload[1:])
	}
	if len(payload) > 0 && payload[0] == 0 {
		return payload[1:], nil
	}
	return payload, nil
}

// UploadForTest exposes one upload for the bench program.
func UploadForTest(payload []byte) (string, error) {
	return upload(newClient(), payload)
}
