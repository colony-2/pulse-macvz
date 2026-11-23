package macvz

import (
	"embed"
)

//go:embed scripts/bootstrap-guest.sh scripts/install-guest.sh scripts/prepare-image.sh scripts/prepare-base.sh scripts/prepare-base-linux.sh scripts/docker-engine.sh scripts/prepare-docker-engine.sh scripts/docker-primary.sh scripts/setup-linux-nix.sh scripts/setup-linux-kvm.sh scripts/configure-linux-gic-traps.py scripts/linux-kvm-network.py scripts/prepare-template.sh scripts/select-c2j.sh scripts/resolve-c2j-vendor.sh scripts/record-c2j-version.sh flake.nix flake.lock guest/*.go guest/logstore/*.go guest/go.mod.txt guest/go.sum.txt tests/integration/c2j-fixture.go.txt tests/integration/docker-fixture.go.txt
var assets embed.FS
