package backend

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gjpin/agent-os/internal/model"
	"github.com/gjpin/agent-os/internal/profile"
)

type limaDiskRunner struct {
	diskJSON string
	calls    []call
}

func (r *limaDiskRunner) Run(_ context.Context, name string, args []string, _ io.Reader, stdout, _ io.Writer) error {
	r.calls = append(r.calls, call{name: name, args: append([]string(nil), args...)})
	if name == "limactl" && len(args) >= 2 && args[0] == "disk" && args[1] == "list" && stdout != nil {
		_, _ = io.WriteString(stdout, r.diskJSON)
	}
	return nil
}

func TestDecodeLimaDisks(t *testing.T) {
	for _, data := range []string{
		`[{"name":"profile","size":10737418240}]`,
		`{"name":"profile","size":10737418240}` + "\n",
	} {
		disks, err := decodeLimaDisks([]byte(data))
		if err != nil || len(disks) != 1 || disks[0].Name != "profile" || limaDiskSizeGiB(disks[0].Size) != 10 {
			t.Fatalf("disks=%+v err=%v", disks, err)
		}
	}
}

func TestLimaProfileCreateResizeAndPurgeUseDiskCommands(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	config := model.DefaultConfig(dir)
	spec := Spec{Config: config}

	createRunner := &limaDiskRunner{diskJSON: `[]`}
	if err := ensureLimaProfile(ctx, createRunner, io.Discard, io.Discard, spec); err != nil {
		t.Fatal(err)
	}
	if got := commandList(createRunner.calls); !strings.Contains(got, "limactl disk create "+profile.DiskID(config.VMName)) {
		t.Fatalf("create calls:\n%s", got)
	}
	metadata, err := profile.NewStore(dir).Load(config.VMName)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Label != profile.DiskLabel(metadata.DiskID) || !strings.HasPrefix(metadata.Label, "agent-os-") {
		t.Fatalf("new metadata=%+v", metadata)
	}

	metadata.SizeGiB = 10
	if err := profile.NewStore(dir).Save(config.VMName, metadata); err != nil {
		t.Fatal(err)
	}
	config.ProfileDiskGiB = 20
	resizeRunner := &limaDiskRunner{diskJSON: fmt.Sprintf(`[{"name":%q,"size":10737418240}]`, metadata.DiskID)}
	if err := ensureLimaProfile(ctx, resizeRunner, io.Discard, io.Discard, Spec{Config: config}); err != nil {
		t.Fatal(err)
	}
	if got := commandList(resizeRunner.calls); !strings.Contains(got, "limactl disk resize "+metadata.DiskID+" --size 20GiB") {
		t.Fatalf("resize calls:\n%s", got)
	}

	purgeRunner := &limaDiskRunner{}
	if err := (Lima{Runner: purgeRunner}).PurgeProfile(ctx, Spec{Config: config}); err != nil {
		t.Fatal(err)
	}
	if got := commandList(purgeRunner.calls); got != "limactl disk delete "+metadata.DiskID {
		t.Fatalf("purge calls=%q", got)
	}
	if _, err := profile.NewStore(dir).Load(config.VMName); !os.IsNotExist(err) {
		t.Fatalf("metadata remains after purge: %v", err)
	}
}

func TestLimaProfileRejectsUntrustedDisk(t *testing.T) {
	config := model.DefaultConfig(t.TempDir())
	diskID := profile.DiskID(config.VMName)
	r := &limaDiskRunner{diskJSON: fmt.Sprintf(`[{"name":%q,"size":10737418240}]`, diskID)}
	if err := ensureLimaProfile(context.Background(), r, io.Discard, io.Discard, Spec{Config: config}); err == nil || !strings.Contains(err.Error(), "without trusted metadata") {
		t.Fatalf("untrusted disk error=%v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("untrusted disk was mutated: %+v", r.calls)
	}
}

func TestExistingLimaPrefixedProfileLabelRemainsCompatible(t *testing.T) {
	dir := t.TempDir()
	config := model.DefaultConfig(dir)
	diskID := profile.DiskID(config.VMName)
	suffix := diskID
	if len(suffix) > 11 {
		suffix = suffix[len(suffix)-11:]
	}
	metadata := profile.Metadata{SchemaVersion: profile.SchemaVersion, Provider: "lima", DiskID: diskID, SizeGiB: config.ProfileDiskGiB, Filesystem: profile.Filesystem, Label: "lima-" + suffix}
	if err := profile.NewStore(dir).Save(config.VMName, metadata); err != nil {
		t.Fatal(err)
	}
	_, loaded, found, err := loadProfile(Spec{Config: config})
	if err != nil || !found || loaded.Label != metadata.Label {
		t.Fatalf("loaded=%+v found=%t err=%v", loaded, found, err)
	}
}

func commandList(calls []call) string {
	lines := make([]string, 0, len(calls))
	for _, call := range calls {
		lines = append(lines, call.name+" "+strings.Join(call.args, " "))
	}
	return strings.Join(lines, "\n")
}
