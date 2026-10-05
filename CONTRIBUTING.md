# Contributing to jsoncap

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- Changing the text of `ErrArrayCap` or `ErrElementBudget`, or the format of the errors that wrap them, needs a matching change in [seadex-scout](https://github.com/cplieger/seadex-scout). The seadex-scout tests search each error for that exact text and fail when seadex-scout next updates jsoncap.
