package main

// ─── jjam_crypto_test.go — the encryption of the upload ─────────────────
//
// The bot uploads the video of a render from a file, so the whole video
// does not have to sit in the memory. The encryption of the upload then
// runs in a stream instead of in one piece.
//
// These tests check that the stream makes exactly the same bytes as the
// encryption of one piece, so the video that WhatsApp gets is the same.

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"testing"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/util/cbcutil"
	"github.com/polymorfa/hypermeow/util/hkdfutil"
)

// jjamMediaKeys is the key of the media, the way the library makes it.
//
// The library keeps this for itself, so the test makes it the same way:
// the key of the media is stretched with the kind of the media.
func jjamMediaKeys(mediaKey []byte, appInfo whatsmeow.MediaType) (iv, cipherKey, macKey []byte) {
	expanded := hkdfutil.SHA256(mediaKey, nil, []byte(appInfo), 112)
	return expanded[:16], expanded[16:48], expanded[48:80]
}

// TestJjamEncryptStream checks that the stream encrypts like one piece.
func TestJjamEncryptStream(t *testing.T) {
	mediaKey := make([]byte, 32)
	if _, err := rand.Read(mediaKey); err != nil {
		t.Fatalf("the key is not random: %v", err)
	}
	iv, cipherKey, macKey := jjamMediaKeys(mediaKey, whatsmeow.MediaVideo)

	// A file that is bigger than the buffer of the stream, so the stream
	// has to do more than one round.
	plaintext := make([]byte, 100*1024)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("the video is not random: %v", err)
	}

	// The way of the library when the whole file is in the memory.
	want, err := cbcutil.Encrypt(cipherKey, iv, plaintext)
	if err != nil {
		t.Fatalf("the video is not encrypted: %v", err)
	}

	// The way of the library when the video comes from a file.
	var buf bytes.Buffer
	plainHash, encHash, size, uploadSize, err := cbcutil.EncryptStream(
		cipherKey, iv, macKey, bytes.NewReader(plaintext), &buf,
	)
	if err != nil {
		t.Fatalf("the video is not encrypted in the stream: %v", err)
	}
	got := buf.Bytes()

	if size != uint64(len(plaintext)) {
		t.Fatalf("the stream says the video is %d bytes, want %d", size, len(plaintext))
	}
	if uploadSize != uint64(len(got)) {
		t.Fatalf("the stream says the upload is %d bytes, want %d", uploadSize, len(got))
	}
	if len(got) != len(want)+10 {
		t.Fatalf("the stream made %d bytes, want %d (the video and the mark)",
			len(got), len(want)+10)
	}
	if !bytes.Equal(got[:len(want)], want) {
		t.Fatal("the stream encrypts the video differently than one piece")
	}
	// The mark of the file is over the cipher and the key of the media.
	h := hmac.New(sha256.New, macKey)
	h.Write(iv)
	h.Write(want)
	mark := h.Sum(nil)[:10]
	if !bytes.Equal(got[len(want):], mark) {
		t.Fatal("the mark of the stream is not the mark of the file")
	}
	// The hashes of the answer are the hashes of the video.
	if !bytes.Equal(plainHash, func() []byte {
		sum := sha256.Sum256(plaintext)
		return sum[:]
	}()) {
		t.Fatal("the hash of the video is not the hash of the video")
	}
	if !bytes.Equal(encHash, func() []byte {
		sum := sha256.Sum256(got)
		return sum[:]
	}()) {
		t.Fatal("the hash of the upload is not the hash of the upload")
	}
}

// TestJjamEncryptStreamEmpty checks that a video of zero bytes still works.
func TestJjamEncryptStreamEmpty(t *testing.T) {
	mediaKey := make([]byte, 32)
	if _, err := rand.Read(mediaKey); err != nil {
		t.Fatalf("the key is not random: %v", err)
	}
	iv, cipherKey, macKey := jjamMediaKeys(mediaKey, whatsmeow.MediaVideo)

	var buf bytes.Buffer
	_, _, size, uploadSize, err := cbcutil.EncryptStream(
		cipherKey, iv, macKey, bytes.NewReader(nil), &buf,
	)
	if err != nil {
		t.Fatalf("an empty video is not encrypted: %v", err)
	}
	if size != 0 {
		t.Fatalf("the stream says the video is %d bytes, want 0", size)
	}
	if uploadSize != uint64(buf.Len()) {
		t.Fatalf("the stream says the upload is %d bytes, want %d", uploadSize, buf.Len())
	}
	// The empty video still has its padding and its mark.
	if buf.Len() != 16+10 {
		t.Fatalf("the upload of an empty video is %d bytes, want 26", buf.Len())
	}
}

// TestJjamEncryptStreamReader checks that the stream reads the video from
// the first byte of the reader.
//
// The bot downloads the video into a file and uploads that file, so the
// file has to be at its first byte when the upload starts.
func TestJjamEncryptStreamReader(t *testing.T) {
	mediaKey := make([]byte, 32)
	if _, err := rand.Read(mediaKey); err != nil {
		t.Fatalf("the key is not random: %v", err)
	}
	iv, cipherKey, macKey := jjamMediaKeys(mediaKey, whatsmeow.MediaVideo)

	plaintext := []byte("the video of the render")
	var buf bytes.Buffer
	_, _, size, _, err := cbcutil.EncryptStream(
		cipherKey, iv, macKey, bytes.NewReader(plaintext), &buf,
	)
	if err != nil {
		t.Fatalf("the video is not encrypted: %v", err)
	}
	if size != uint64(len(plaintext)) {
		t.Fatalf("the stream says the video is %d bytes, want %d", size, len(plaintext))
	}
	// The stream has to read everything, not a part of it.
	if _, err := io.ReadAll(bytes.NewReader(plaintext)); err != nil {
		t.Fatalf("the video is not readable: %v", err)
	}
}
