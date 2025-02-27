# golang.org/x/build/cmd/releasebot

Command releasebot runs a Go release.

The release happens in two stages:

* the `prepare` stage checks preconditions, makes the release commit and mails it for review (if applicable), and performs testing;
* the `release` stage runs after the release commit (if any) is merged, and it tags, builds and cleans up the release.

## Permissions

The user running a release will need:

* A GitHub personal access token with the `public_repo` scope in `~/.github-issue-token`, and an account with write access to golang/go
* gomote access and a token in your name
* gcloud application default credentials, and an account with GCS access to golang-org for bucket golang-release-staging
* **`release-manager` group membership on Gerrit**
* for `-mode=mail-dl-cl` only, Secret Manager access to Gerrit API secret
* for `-mode=tweet-*` only, Secret Manager access to Twitter API secret

NOTE: all but the Gerrit permission are ensured by the bot on startup.
