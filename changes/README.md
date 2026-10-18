# Release note fragments

A pull request that needs more than its title in the changelog adds one file
here, for example upgrade notes for CAPRF, a breaking change or a behaviour
change. Most pull requests do not need a file: the squash-merged pull request
title becomes the changelog line.

Name the file after the pull request or the topic (`changes/812.md`,
`changes/standby-long-poll.md`). The front matter names the `CHANGELOG.md`
section, the text is Markdown:

```markdown
---
section: Upgrade notes for CAPRF
---

- Standby long polls `COMMANDS_URL` for 20 s. A proxy with an idle timeout
  under 20 s breaks standby at boot (#716).
```

A fragment does not replace the line from the pull request title: both are in
the release section, the fragment in its own section. Name the pull request in
the fragment text, as above, so that readers can connect the two.

Common sections: `Breaking changes`, `Behaviour changes in all modes`,
`Upgrade notes for CAPRF`, `Highlights`. Another name makes a new section,
and `make changelog` prints a warning for it.

`make changelog` prints the next release section with the fragments. The
release commit (`make changelog-release`) writes them into `CHANGELOG.md` and
deletes them.
