# Reference formula for homebrew-core submission.
# This file documents the expected formula structure. To submit:
# 1. Fork homebrew/homebrew-core
# 2. Copy this formula to Formula/q/qurl.rb
# 3. Update the url and sha256 for the target release
# 4. Run `brew audit --strict --new qurl` and fix any issues
# 5. Run `brew install --build-from-source qurl` to verify
# 6. Submit a PR to homebrew/homebrew-core
#
# Until accepted into homebrew-core, users can install via tap:
#   brew tap layervai/tap
#   brew install qurl

class Qurl < Formula
  desc "Manage secure links from the command line"
  homepage "https://layerv.ai"
  # Update url and sha256 for each release:
  url "https://github.com/layervai/qurl-integrations/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "UPDATE_WITH_ACTUAL_SHA256"
  license "MIT"
  head "https://github.com/layervai/qurl-integrations.git", branch: "main"

  depends_on "go" => :build

  def install
    ldflags = %W[
      -s -w
      -X main.version=#{version}
    ]
    system "go", "build", *std_go_args(ldflags:), "./apps/cli/cmd/"

    # Shell completions (bash, zsh, fish)
    generate_completions_from_executable(bin/"qurl", "completion")

    # Man pages
    mkdir_p "man"
    system bin/"qurl", "docs", "man", "-d", "man"
    man1.install Dir["man/*.1"]
  end

  test do
    # Version check (offline, no API key needed)
    assert_match version.to_s, shell_output("#{bin}/qurl version")

    # Verify missing API key produces a helpful error, not a crash
    output = shell_output("#{bin}/qurl list 2>&1", 1)
    assert_match "API key required", output

    # Verify config path is deterministic
    assert_match ".config/qurl", shell_output("#{bin}/qurl config path")

    # Verify JSON output flag is accepted
    output = shell_output("#{bin}/qurl list -o json 2>&1", 1)
    assert_match "API key required", output
  end
end
