# Release Checklist

description: How to cut a signed ZeoKeel Rig release.

## Checklist

1. Run `go test ./... -count=1 -race`.
2. Update CHANGELOG.md and README.md.
3. Tag the release and push the tag.
4. Verify the module builds with the release tag.
