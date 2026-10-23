package server

import (
	"cmp"
	"net/http"
	"strings"

	"github.com/nostosx/kitx/dataurl"

	"github.com/nostosx/agentx/internal/media"

	"github.com/nostosx/agentx/model"
)

// This file converts the files of user messages, in the Vercel UI message,
// A2A and AG-UI formats, to model parts.

// acceptedMedia lists the media types of the files user messages may
// carry, for errors and the A2A agent card. Types ending in "/*" stand for
// all of their subtypes.
var acceptedMedia = []string{
	"image/*", "audio/*", "video/*", "text/*", "application/pdf",
	"application/msword", "application/vnd.ms-excel", "application/vnd.openxmlformats-officedocument.*",
}

// accepted reports whether the server takes a file of mediaType as a model
// part. Whether a provider takes it depends on the provider and model; one
// that cannot fails the run.
func accepted(mediaType string) bool {
	mt := media.BaseType(mediaType)
	for _, a := range acceptedMedia {
		prefix, wild := strings.CutSuffix(a, "*")
		if wild && strings.HasPrefix(mt, prefix) && len(mt) > len(prefix) || !wild && mt == a {
			return true
		}
	}
	return false
}

func errUnsupportedMedia(mediaType string) *Error {
	return errorf(http.StatusUnsupportedMediaType, CodeUnsupportedMedia, "media type %q is not supported; supported: %s",
		mediaType, strings.Join(acceptedMedia, ", "))
}

// filePart returns the model part of a file a client sent: data, a data:
// URL or another URL, with its media type and file name, either of which
// may be empty. The media type is the one given, else that of the data:
// URL, else guessed from the file name or URL; the part's type follows it
// (see model.MediaPart). Media types that are not accepted are errors.
func filePart(mediaType string, data []byte, url, filename string) (model.Part, *Error) {
	if len(data) == 0 && strings.HasPrefix(strings.ToLower(strings.TrimSpace(url)), "data:") {
		mt, d, err := dataurl.Decode(url)
		if err != nil {
			return model.Part{}, errorf(http.StatusBadRequest, CodeBadRequest, "invalid data URL")
		}
		data, url, mediaType = d, "", cmp.Or(mediaType, mt)
	}
	if len(data) == 0 && url == "" {
		return model.Part{}, errorf(http.StatusBadRequest, CodeBadRequest, "a file part has no data or URL")
	}
	mediaType = cmp.Or(mediaType, media.ExtensionType(filename), media.ExtensionType(url))
	if !accepted(mediaType) {
		return model.Part{}, errUnsupportedMedia(mediaType)
	}
	p := model.Part{Type: model.MediaPartType(mediaType), MediaType: mediaType, Data: data, URL: url}
	if p.Type != model.PartImage {
		p.Filename = filename
	}
	return p, nil
}
