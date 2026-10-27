# ZFS integration tests need root and a dataset they may own. The test binary
# is built as you and only executed under sudo.
SBX_TEST_ZFS_ROOT ?= tank/sbx-test
SUDO_TEST = go test -exec 'sudo --preserve-env=SBX_TEST_ZFS_ROOT' -count=1

.PHONY: test test-zfs bench-zfs vet

test:
	go test ./...

test-zfs:
	SBX_TEST_ZFS_ROOT=$(SBX_TEST_ZFS_ROOT) $(SUDO_TEST) -v ./internal/zfs/

bench-zfs:
	SBX_TEST_ZFS_ROOT=$(SBX_TEST_ZFS_ROOT) $(SUDO_TEST) -run '^$$' -bench . -benchtime 200x ./internal/zfs/

vet:
	go vet ./...
