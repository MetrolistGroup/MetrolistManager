package buildconfig

import (
	"strings"
	"testing"
)

func TestValidateConfiguration(t *testing.T) {
	valid := configuration{
		targetVersion:    "14.0.0",
		releaseTag:       "v14.0.0",
		desktopAssetName: "Metrolist-Desktop-windows.zip",
		repositoryOwner:  "MetrolistGroup",
		repositoryName:   "Metrolist",
	}
	if err := validate(valid); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
	tests := []struct {
		name    string
		change  func(*configuration)
		wantErr string
	}{
		{"version", func(c *configuration) { c.targetVersion = "14.0" }, "target version"},
		{"asset", func(c *configuration) { c.desktopAssetName = "wrong.zip" }, "desktop asset"},
		{"tag", func(c *configuration) { c.releaseTag = "14.0.0" }, "release tag"},
		{"prerelease", func(c *configuration) { c.targetVersion = "14.0.0-a5" }, "stable"},
		{"repository", func(c *configuration) { c.repositoryOwner = "" }, "repository"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.change(&config)
			if err := validate(config); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validate() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestCurrentConfiguration(t *testing.T) {
	err := Validate()
	if TargetVersion == "0.0.0-dev" {
		if err == nil || !strings.Contains(err.Error(), "development build") {
			t.Fatalf("Validate() error = %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}
