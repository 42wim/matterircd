package matrixclient

import (
	"context"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Upload uploads data to the media repository and returns its mxc:// URI.
func (c *Client) Upload(ctx context.Context, data []byte, filename, contentType string) (id.ContentURI, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.mc.UploadBytesWithName(ctx, data, contentType, filename)
		if err == nil {
			return resp.ContentURI, nil
		}

		retry, herr := c.HandleRetry(ctx, "Upload", err, attempt, maxAPIRetries)
		if herr != nil {
			return id.ContentURI{}, herr
		}

		if !retry {
			return id.ContentURI{}, err
		}
	}
}

// SendFile uploads data and sends it as a file message, with caption as the
// accompanying text when set. It is a thread reply when parentID is set.
func (c *Client) SendFile(ctx context.Context, roomID id.RoomID, parentID id.EventID, filename, contentType, caption string, data []byte) (id.EventID, error) {
	uri, err := c.Upload(ctx, data, filename, contentType)
	if err != nil {
		return "", err
	}

	content := event.MessageEventContent{
		MsgType: msgTypeFor(contentType),
		Body:    filename,
		URL:     uri.CUString(),
		Info:    &event.FileInfo{MimeType: contentType, Size: len(data)},
	}

	if caption != "" {
		content.FileName = filename
		content.Body = caption
	}

	if parentID != "" {
		content.RelatesTo = &event.RelatesTo{Type: event.RelThread, EventID: parentID}
	}

	resp, err := c.mc.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
	if err != nil {
		return "", err
	}

	return resp.EventID, nil
}

// Download fetches the media behind an mxc:// URI.
func (c *Client) Download(ctx context.Context, mxc id.ContentURIString) ([]byte, error) {
	uri, err := mxc.Parse()
	if err != nil {
		return nil, err
	}

	for attempt := 0; ; attempt++ {
		data, err := c.mc.DownloadBytes(ctx, uri)
		if err == nil {
			return data, nil
		}

		retry, herr := c.HandleRetry(ctx, "Download", err, attempt, maxAPIRetries)
		if herr != nil {
			return nil, herr
		}

		if !retry {
			return nil, err
		}
	}
}

// DownloadEncrypted fetches and decrypts the media of an encrypted file message.
func (c *Client) DownloadEncrypted(ctx context.Context, file *event.EncryptedFileInfo) ([]byte, error) {
	data, err := c.Download(ctx, file.URL)
	if err != nil {
		return nil, err
	}

	err = file.DecryptInPlace(data)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// DownloadURL returns the HTTP URL of the media behind an mxc:// URI, or "".
func (c *Client) DownloadURL(mxc id.ContentURIString) string {
	uri, err := mxc.Parse()
	if err != nil {
		return ""
	}

	return c.mc.BuildURL(mautrix.MediaURLPath{"v1", "media", "download", uri.Homeserver, uri.FileID})
}

// PublicLink returns an unauthenticated download URL for an mxc:// URI, usable
// by anyone with the link. Homeservers that only allow authenticated media
// refuse it, use DownloadURL with an access token there. Returns "" if mxc is invalid.
func (c *Client) PublicLink(mxc id.ContentURIString) string {
	uri, err := mxc.Parse()
	if err != nil {
		return ""
	}

	return c.mc.BuildURL(mautrix.MediaURLPath{"v3", "download", uri.Homeserver, uri.FileID})
}

// PublicLinks is PublicLink for multiple URIs, skipping invalid ones.
func (c *Client) PublicLinks(mxcs []id.ContentURIString) []string {
	links := make([]string, 0, len(mxcs))

	for _, mxc := range mxcs {
		link := c.PublicLink(mxc)
		if link != "" {
			links = append(links, link)
		}
	}

	return links
}

func msgTypeFor(contentType string) event.MessageType {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return event.MsgImage
	case strings.HasPrefix(contentType, "audio/"):
		return event.MsgAudio
	case strings.HasPrefix(contentType, "video/"):
		return event.MsgVideo
	}

	return event.MsgFile
}
