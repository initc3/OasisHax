// +build e2ecoverage

package main

import (
	"testing"

	cmnTesting "github.com/oasislabs/oasis-core/go/common/testing"
)

func TestCoverageE2E(t *testing.T) {
	cmnTesting.RunMain(t, main)
}