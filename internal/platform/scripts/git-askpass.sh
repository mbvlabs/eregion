#!/bin/sh
# Git credential helper used only for a private HTTPS clone. The token is read
# from the child-process environment and never written to argv, the journal, or
# events. Git invokes this with a prompt string as the only argument.
case "$1" in
    *[Uu]sername*)
        printf '%s\n' "${EREGION_GIT_USERNAME:-x-access-token}"
        ;;
    *)
        printf '%s\n' "${EREGION_GIT_PASSWORD:-}"
        ;;
esac
