package main

// upscale.go — .upscale: reply video/foto → upscale ke 8K + 120fps.
//
//   Video: scale max 8K (7680×4320, lanczos) + interpolasi motion ke 120fps
//     (minterpolate, hanya bila sumber <120fps) + sharpening ringan → H.265.
//   Foto: scale max 8K + unsharp → JPEG kualitas 95.
//
// CATATAN JUJUR: tanpa AI upscaler, upscale tidak menambah detail asli —
// hanya memperbesar dengan algoritma terbaik (lanczos) + sharpening agar
// terlihat lebih tajam. Proses 8K+120fps berat di CPU (bisa belasan menit
// untuk video pendek) dan file hasilnya besar.
//
// Contoh: reply video/foto lalu kirim .upscale

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// upscaleRunFFmpeg menjalankan ffmpeg dengan timeout 45 menit (8K berat).
func upscaleRunFFmpeg(args ...string) error {
	ffCtx, ffCancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer ffCancel()
	cmd := exec.CommandContext(ffCtx, args[0], args[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %v (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// upscaleVideo: denoise → deblock → 8K lanczos max → 120fps → CAS sharpen
// → H.265 CRF 20. Pipeline mentok kualitas tanpa AI.
func upscaleVideo(srcPath string, srcFps float64) (string, error) {
	out, err := os.CreateTemp("", "wabot-upscale-*.mp4")
	if err != nil {
		return "", err
	}
	outPath := out.Name()
	out.Close()

	var filters []string
	// Bersihkan dulu SEBELUM diperbesar (noise & blok ikut membesar)
	filters = append(filters, "hqdn3d=2:2:6:6", "deblock")
	filters = append(filters, "scale='min(7680,iw)':'min(4320,ih)':eval=frame:flags=lanczos+accurate_rnd+full_chroma_int")
	if srcFps < 119.0 {
		// Interpolasi motion ke 120fps (berat di CPU)
		filters = append(filters, "minterpolate=fps=120:mi_mode=mci:me_mode=bidir:vsbmc=1")
	}
	// CAS = contrast-adaptive sharpening, jauh lebih tajam dari unsharp biasa
	filters = append(filters, "cas=0.7", "eq=contrast=1.03:saturation=1.08", "format=yuv420p")
	vf := strings.Join(filters, ",")

	err = upscaleRunFFmpeg("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", srcPath,
		"-vf", vf,
		"-c:v", "libx265", "-crf", "20", "-preset", "medium",
		"-tag:v", "hvc1",
		"-c:a", "aac", "-b:a", "128k",
		"-movflags", "+faststart",
		outPath,
	)
	if err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	if st, e := os.Stat(outPath); e != nil || st.Size() == 0 {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("hasil upscale kosong")
	}
	return outPath, nil
}

// upscalePhoto: PIPELINE MENTOK TOTAL (foto only, video tidak disentuh).
// deblock → nlmeans (non-local means, denoiser kelas atas) → zscale
// spline36 (aspek rasio ASLI dijaga via expression, max 8K) → CAS max → micro-contrast
// → JPEG q98, dikirim sebagai foto biasa.
func upscalePhoto(srcPath string) (string, error) {
	// Fit dalam box 8K dengan aspek rasio asli — dihitung di DALAM ffmpeg
	// dari dimensi frame hasil decode (sudah auto-rotate), jadi kebal
	// terhadap metadata rotasi yang bikin hasil lonjong.
	fitW := "trunc(min(7680/iw\\,4320/ih)*iw/2)*2"
	fitH := "trunc(min(7680/iw\\,4320/ih)*ih/2)*2"

	out, err := os.CreateTemp("", "wabot-upscale-*.jpg")
	if err != nil {
		return "", err
	}
	outPath := out.Name()
	out.Close()

	vf := strings.Join([]string{
		"deblock",
		// nlmeans di ukuran sumber (lebih cepat + lebih akurat)
		"nlmeans=s=4:p=7:r=15",
		// Normalisasi format dulu — zscale gagal ("no path between
		// colorspaces") kalau metadata colorspace sumbernya aneh
		"format=yuv420p",
		// spline36 via zscale: filter upscale terbaik untuk foto
		"zscale=" + fitW + ":" + fitH + ":filter=spline36",
		"cas=1.0",
		"unsharp=5:5:0.3:5:5:0.0",
		"eq=contrast=1.05:saturation=1.1",
	}, ",")
	err = upscaleRunFFmpeg("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", srcPath,
		"-vf", vf,
		"-q:v", "1",
		outPath,
	)
	if err != nil {
		_ = os.Remove(outPath)
		return "", err
	}
	if st, e := os.Stat(outPath); e != nil || st.Size() == 0 {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("hasil upscale kosong")
	}
	return outPath, nil
}

func handleUpscale(client *whatsmeow.Client, msg *events.Message, args string) string {
	if client == nil {
		return "❌ Client tidak siap"
	}
	quoted := getQuotedMessage(msg)
	if quoted == nil || quoted.Message == nil {
		return "❓ *Cara pakai:* reply sebuah video/foto lalu kirim *.upscale*"
	}
	qm := quoted.Message

	// Tentukan tipe: video atau gambar (termasuk via dokumen)
	var dlMsg whatsmeow.DownloadableMessage
	isVideo := false
	if vm := qm.GetVideoMessage(); vm != nil {
		dlMsg, isVideo = vm, true
	} else if dwc := qm.GetDocumentWithCaptionMessage(); dwc != nil {
		if vm := dwc.GetMessage().GetVideoMessage(); vm != nil {
			dlMsg, isVideo = vm, true
		} else if doc := dwc.GetMessage().GetDocumentMessage(); doc != nil {
			if strings.HasPrefix(doc.GetMimetype(), "video/") {
				dlMsg, isVideo = doc, true
			} else if strings.HasPrefix(doc.GetMimetype(), "image/") {
				dlMsg = doc
			}
		}
	} else if doc := qm.GetDocumentMessage(); doc != nil {
		if strings.HasPrefix(doc.GetMimetype(), "video/") {
			dlMsg, isVideo = doc, true
		} else if strings.HasPrefix(doc.GetMimetype(), "image/") {
			dlMsg = doc
		}
	} else if img := qm.GetImageMessage(); img != nil {
		dlMsg = img
	}
	if dlMsg == nil {
		return "❓ *Cara pakai:* reply sebuah video/foto lalu kirim *.upscale*"
	}

	// Download
	dlCtx, dlCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	data, dlErr := client.Download(dlCtx, dlMsg)
	dlCancel()
	if dlErr != nil {
		return fmt.Sprintf("❌ Gagal download: %v", dlErr)
	}
	if len(data) == 0 {
		return "❌ Media kosong."
	}
	src, err := os.CreateTemp("", "wabot-upscale-src-*")
	if err != nil {
		return fmt.Sprintf("❌ Gagal buat temp: %v", err)
	}
	srcPath := src.Name()
	if _, err := src.Write(data); err != nil {
		src.Close()
		_ = os.Remove(srcPath)
		return fmt.Sprintf("❌ Gagal tulis temp: %v", err)
	}
	src.Close()
	defer os.Remove(srcPath)
	origSize := int64(len(data))

	// Proses
	var outPath string
	var aksi string
	if isVideo {
		_, _, _, fps, _, perr := probeVideoFull(srcPath)
		if perr != nil {
			fps = 30
		}
		outPath, err = upscaleVideo(srcPath, fps)
		aksi = "8K + 120fps + H.265 CRF20 (denoise+CAS)"
	} else {
		outPath, err = upscalePhoto(srcPath)
		aksi = "8K MENTOK (nlmeans+spline36+CAS max, aspek asli, JPEG q98)"
	}
	if err != nil {
		return fmt.Sprintf("❌ Gagal upscale: %v", err)
	}
	defer os.Remove(outPath)

	outData, err := os.ReadFile(outPath)
	if err != nil {
		return fmt.Sprintf("❌ Gagal baca hasil: %v", err)
	}

	// Kirim balik
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	caption := fmt.Sprintf("✅ %s → %s\n%s", formatMB(origSize), formatMB(int64(len(outData))), aksi)
	if isVideo {
		if err := sprSendVideo(client, ctx, msg.Info.Chat, outData, "video/mp4", caption); err != nil {
			return fmt.Sprintf("❌ Gagal kirim: %v", err)
		}
	} else {
		// Foto: kirim sebagai foto biasa (bukan dokumen), JPEG q98
		if err := sprSendImage(client, ctx, msg.Info.Chat, outData, "image/jpeg", caption); err != nil {
			return fmt.Sprintf("❌ Gagal kirim: %v", err)
		}
	}
	return ""
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "upscale",
		Aliases:     []string{"up", "hdkan"},
		Description: "Upscale video/foto ke 8K 120fps (H.265)",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleUpscale(client, msg, args)
		},
	})
}
