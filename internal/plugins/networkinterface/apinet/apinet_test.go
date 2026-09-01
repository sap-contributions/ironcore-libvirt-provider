// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package apinet_test

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ironcore-dev/libvirt-provider/internal/plugins/networkinterface/apinet"
)

var _ = Describe("SuperviseCache", func() {
	It("surfaces a terminal cache error on the returned channel", func() {
		wantErr := errors.New("informer cache stopped with error")

		errCh := apinet.SuperviseCache(context.Background(), logr.Discard(), func(ctx context.Context) error {
			return wantErr
		})

		var gotErr error
		Eventually(errCh, 5*time.Second).Should(Receive(&gotErr))
		Expect(gotErr).To(MatchError(wantErr))
	})

	It("surfaces nothing when the cache stops cleanly (e.g. graceful shutdown)", func() {
		errCh := apinet.SuperviseCache(context.Background(), logr.Discard(), func(ctx context.Context) error {
			return nil
		})

		// A clean stop must not send an error, otherwise the app's errgroup would
		// exit non-zero on every normal shutdown.
		Consistently(errCh, 200*time.Millisecond).ShouldNot(Receive())
	})
})
