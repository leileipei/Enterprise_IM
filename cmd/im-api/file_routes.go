package main

import (
	"errors"
	"net/http"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func assembleFileRoutes(next http.Handler, auth httpserver.Authenticator, repo policystore.Service, admin access.Service, rt *fileRuntime) (h http.Handler, err error) {
	if next == nil || auth == nil || rt == nil || (rt.uploadEnabled && rt.transfer == nil) || (rt.business.Enabled && rt.download == nil) {
		return nil, errors.New("file runtime assembly unavailable")
	}
	h = next
	if rt.business.Enabled {
		h, err = httpserver.HandlerWithFileMessages(h, auth, repo, repo)
	} else {
		h, err = httpserver.HandlerWithConversations(h, auth, repo)
	}
	if err != nil {
		return nil, err
	}
	h, err = httpserver.HandlerWithFileRetentionPolicy(h, auth, admin)
	if err != nil {
		return nil, err
	}
	if rt.uploadEnabled || rt.business.Enabled {
		h, err = httpserver.HandlerWithFileUploadPolicy(h, auth, admin)
		if err != nil {
			return nil, err
		}
		if rt.uploadEnabled {
			h, err = httpserver.HandlerWithFileMetadata(h, auth, repo)
		} else {
			h, err = httpserver.HandlerWithFileStatus(h, auth, repo)
		}
		if err != nil {
			return nil, err
		}
	}
	if rt.uploadEnabled {
		h, err = httpserver.HandlerWithFileContent(h, auth, rt.transfer)
		if err != nil {
			return nil, err
		}
	}
	if rt.business.Enabled {
		h, err = httpserver.HandlerWithFileDownload(h, auth, rt.download)
		if err != nil {
			return nil, err
		}
		h, err = httpserver.HandlerWithFileSearch(h, auth, repo)
		if err != nil {
			return nil, err
		}
	} else {
		h = productionFileSearchHandler(h, auth)
	}
	h = httpserver.HandlerWithFileContentModes(h, rt.uploadEnabled, rt.business.Enabled)
	return httpserver.HandlerWithFileCapabilities(h, auth, admin, productionFileCapabilities(rt.uploadEnabled, rt.business.Enabled))
}
