//go:build integration

package agentsession_test

import (
	"os"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.RunPackage(m)) }
