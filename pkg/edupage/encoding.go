package edupage

import (
	"bytes"
	"compress/flate"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
)

// Pair is one form field. EduPage's scripts send fields in a fixed order, so forms are lists, not maps.
type Pair struct{ Key, Value string }

// quote escapes like Python's urllib.parse.quote, which edupage.org's own form encoder matches:
// spaces become %20 and slashes stay.
func quote(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(s), "+", "%20"), "%2F", "/")
}

func encodeForm(pairs []Pair) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = quote(p.Key) + "=" + quote(p.Value)
	}
	return strings.Join(parts, "&")
}

// EncodeRequestBody wraps a form the way edubarUtils.js does for its RPC calls: the form is deflated
// (raw, no zlib header), base64-encoded and prefixed with "dz:"; eqacs is the SHA-1 of that value.
func EncodeRequestBody(pairs []Pair) string {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	_, _ = w.Write([]byte(encodeForm(pairs)))
	_ = w.Close()
	eqap := "dz:" + base64.StdEncoding.EncodeToString(buf.Bytes())
	sum := sha1.Sum([]byte(eqap))
	return encodeForm([]Pair{{"eqap", eqap}, {"eqacs", hex.EncodeToString(sum[:])}, {"eqaz", "1"}})
}

// DecodeRequestBody is EncodeRequestBody backwards, for the fake server and tests.
func DecodeRequestBody(body string) (url.Values, error) {
	outer, err := url.ParseQuery(body)
	if err != nil {
		return nil, err
	}
	eqap := outer.Get("eqap")
	if !strings.HasPrefix(eqap, "dz:") {
		return outer, nil
	}
	sum := sha1.Sum([]byte(eqap))
	if outer.Get("eqacs") != hex.EncodeToString(sum[:]) {
		return nil, errors.New("eqacs does not match eqap")
	}
	raw, err := base64.StdEncoding.DecodeString(eqap[3:])
	if err != nil {
		return nil, err
	}
	form, err := io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil, err
	}
	return url.ParseQuery(string(form))
}

// DecodeResponse undoes EduPage's RPC answer wrapping: "eqz:" or "eqwd:" and base64, else plain text.
func DecodeResponse(text string) ([]byte, error) {
	for _, prefix := range []string{"eqz:", "eqwd:"} {
		if rest, ok := strings.CutPrefix(text, prefix); ok {
			raw, err := base64.StdEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(rest), "="))
			if err != nil {
				raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(rest), "="))
			}
			if err != nil {
				return nil, err
			}
			if json.Valid(raw) {
				return raw, nil
			}
			// Some answers are deflated as well.
			if inflated, ierr := io.ReadAll(flate.NewReader(bytes.NewReader(raw))); ierr == nil {
				return inflated, nil
			}
			return raw, nil
		}
	}
	return []byte(text), nil
}

// EncodeResponse wraps an RPC answer like EduPage does; for the fake server.
func EncodeResponse(data []byte) string {
	return "eqz:" + base64.StdEncoding.EncodeToString(data)
}

// pyJSON is json.Marshal without HTML escaping, the way Python's json.dumps leaves < > & alone.
func pyJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(buf.String(), "\n")
}
