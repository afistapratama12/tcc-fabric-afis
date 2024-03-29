/*
Copyright IBM Corp All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dlog

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/hyperledger-labs/fabric-token-sdk/integration"
)

func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	RegisterFailHandler(Fail)
	RunSpecs(t, "EndToEnd NFT (DLOG) Suite Orion")
}

func StartPortDlog() int {
	return integration.OrionZKATDLogNFT.StartPortForNode()
}
