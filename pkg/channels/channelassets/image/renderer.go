package image

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets"
)

func (r *Renderer) Render(ctx context.Context, in channelassets.Input) (channelassets.Output, error) {
	if int64(len(in.Payload)) > maxInput {
		return channelassets.Output{}, fmt.Errorf("input %d bytes > max %d", len(in.Payload), maxInput)
	}
	if err := ctx.Err(); err != nil {
		return channelassets.Output{}, err
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(in.Payload))
	if err != nil {
		return channelassets.Output{}, fmt.Errorf("not a decodable image: %w", err)
	}
	if cfg.Width > maxDimension || cfg.Height > maxDimension {
		return channelassets.Output{}, fmt.Errorf("image %dx%d exceeds max %d per side", cfg.Width, cfg.Height, maxDimension)
	}

	var out bytes.Buffer
	var mime, ext string
	switch format {
	case "png":
		img, derr := png.Decode(bytes.NewReader(in.Payload))
		if derr != nil {
			return channelassets.Output{}, fmt.Errorf("decode png: %w", derr)
		}
		if eerr := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&out, img); eerr != nil {
			return channelassets.Output{}, fmt.Errorf("encode png: %w", eerr)
		}
		mime, ext = "image/png", ".png"
	case "jpeg":
		img, derr := jpeg.Decode(bytes.NewReader(in.Payload))
		if derr != nil {
			return channelassets.Output{}, fmt.Errorf("decode jpeg: %w", derr)
		}
		if eerr := jpeg.Encode(&out, img, &jpeg.Options{Quality: jpegQuality}); eerr != nil {
			return channelassets.Output{}, fmt.Errorf("encode jpeg: %w", eerr)
		}
		mime, ext = "image/jpeg", ".jpg"
	case "gif":
		g, derr := gif.DecodeAll(bytes.NewReader(in.Payload))
		if derr != nil {
			return channelassets.Output{}, fmt.Errorf("decode gif: %w", derr)
		}
		if eerr := gif.EncodeAll(&out, g); eerr != nil {
			return channelassets.Output{}, fmt.Errorf("encode gif: %w", eerr)
		}
		mime, ext = "image/gif", ".gif"
	default:
		return channelassets.Output{}, fmt.Errorf("unsupported image format %q (png/jpeg/gif only)", format)
	}

	if int64(out.Len()) > maxOutput {
		return channelassets.Output{}, fmt.Errorf("output %d bytes > max %d", out.Len(), maxOutput)
	}
	return channelassets.Output{
		Bytes:    out.Bytes(),
		MIME:     mime,
		Filename: ensureExt(in.Filename, ext),
		AltText:  in.AltText,
	}, nil
}

func ensureExt(name, ext string) string {
	if name == "" {
		return "image" + ext
	}
	if strings.HasSuffix(strings.ToLower(name), ext) {
		return name
	}
	return name + ext
}
