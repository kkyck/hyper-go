package main

/**
 * compat_media.go — helper pengiriman media (audio/video/image/document)
 * ════════════════════════════════════════════════════════════
 * Dipakai bersama oleh plugin downloader, kompres, upscale, tiktok, dan
 * 命令 lain yang butuh mengirim file hasil unduhan ke chat.
 *
 * Fungsi-fungsi di sini sengaja TIDAK mendaftarkan command apa pun —
 * murni helper. Command downloader lived di owner/ atau plugins/ terpisah.
 */

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"google.golang.org/protobuf/proto"
)

const (
	getMaxFileSize = 50 * 1024 * 1024 // 50MB WhatsApp limit
	getDownloadTO  = 5 * time.Minute
	getUploadTO    = 5 * time.Minute
	getMaxFiles    = 10
	getMaxTextLen  = 4000 // max karakter untuk kirim sebagai teks
)

func sprSendMediaFile(client *whatsmeow.Client, chat types.JID, data []byte, mime string, fname string) error {
	ctx, cancel := context.WithTimeout(context.Background(), getUploadTO)
	defer cancel()

	mime = strings.ToLower(mime)
	mime = strings.Split(mime, ";")[0] // strip "; charset=utf-8" etc
	mime = strings.TrimSpace(mime)

	switch {
	case strings.HasPrefix(mime, "audio/"):
		return sprSendAudio(client, ctx, chat, data, mime, fname)
	case strings.HasPrefix(mime, "video/"):
		return sprSendVideo(client, ctx, chat, data, mime, fname)
	case strings.HasPrefix(mime, "image/"):
		return sprSendImage(client, ctx, chat, data, mime, fname)
	default:
		return sprSendDocument(client, ctx, chat, data, mime, fname)
	}
}

// sprSendAudio uploads + sends audio message.
func sprSendAudio(client *whatsmeow.Client, ctx context.Context, chat types.JID, data []byte, mime, fname string) error {
	up, err := client.Upload(ctx, data, whatsmeow.MediaAudio)
	if err != nil {
		return fmt.Errorf("upload audio: %w", err)
	}
	audMsg := &waE2E.Message{
		AudioMessage: &waE2E.AudioMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			// PTT: false (not voice note)
		},
	}
	_, err = client.SendMessage(ctx, chat, audMsg)
	return err
}

// sprSendVideo uploads + sends video message.
func sprSendVideo(client *whatsmeow.Client, ctx context.Context, chat types.JID, data []byte, mime, fname string) error {
	up, err := client.Upload(ctx, data, whatsmeow.MediaVideo)
	if err != nil {
		return fmt.Errorf("upload video: %w", err)
	}
	vidMsg := &waE2E.Message{
		VideoMessage: &waE2E.VideoMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			Caption:       proto.String(fname),
		},
	}
	_, err = client.SendMessage(ctx, chat, vidMsg)
	return err
}

// sprSendImage uploads + sends image message.
func sprSendImage(client *whatsmeow.Client, ctx context.Context, chat types.JID, data []byte, mime, fname string) error {
	up, err := client.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return fmt.Errorf("upload image: %w", err)
	}
	imgMsg := &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			Caption:       proto.String(fname),
		},
	}
	_, err = client.SendMessage(ctx, chat, imgMsg)
	return err
}

// sprSendDocument uploads + sends document message (fallback for any type).
func sprSendDocument(client *whatsmeow.Client, ctx context.Context, chat types.JID, data []byte, mime, fname string) error {
	up, err := client.Upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return fmt.Errorf("upload document: %w", err)
	}
	docMsg := &waE2E.Message{
		DocumentMessage: &waE2E.DocumentMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			FileName:      proto.String(fname),
		},
	}
	_, err = client.SendMessage(ctx, chat, docMsg)
	return err
}

// keep imports used
var _ = os.Stat
