package control

import (
	"bytes"
	"io"
	"mime/multipart"
	"strings"
	"testing"
)

func TestSingleSystemUpdatePackageReaderRejectsAdditionalParts(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	packagePart, err := writer.CreateFormFile("package", "update.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := packagePart.Write([]byte("zip-body")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("unexpected", "value"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	part, err := nextPackagePart(reader)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(&singlePackageReader{part: part, multipart: reader})
	if err == nil || !strings.Contains(err.Error(), "more than one part") {
		t.Fatalf("additional multipart field was not rejected: %v", err)
	}
}
