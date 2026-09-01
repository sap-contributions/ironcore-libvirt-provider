// SPDX-FileCopyrightText: 2023 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"net/http"

	"github.com/go-logr/logr"
	libvirtutils "github.com/ironcore-dev/libvirt-provider/internal/libvirt/utils"
	"k8s.io/apiserver/pkg/server/healthz"
)

type HealthCheck struct {
	Libvirt libvirtutils.Connector
	Log     logr.Logger

	// Checkers are additional named health checks that must all pass for the
	// handler to report healthy (e.g. the apinet kubeconfig rotator).
	Checkers []healthz.HealthChecker
}

func (h HealthCheck) HealthCheckHandler(w http.ResponseWriter, r *http.Request) {
	healthy := true
	if err := libvirtutils.IsConnected(h.Libvirt); err != nil {
		h.Log.Error(err, "failed to get active connection to libvirtd")
		healthy = false
	}

	for _, checker := range h.Checkers {
		if checker == nil {
			continue
		}
		if err := checker.Check(r); err != nil {
			h.Log.Error(err, "health check failed", "check", checker.Name())
			healthy = false
		}
	}

	if healthy {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}
