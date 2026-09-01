// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package apinet_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAPInet(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "APInet Network Interface Plugin Suite")
}
