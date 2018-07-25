#!/bin/bash

# clone-pulumi-repo clones a repository and checkes out the coresponding branch for this composed build. If the repository
# does not have a matching branch, "master" is used. The repository is cloned into GOPATH at the correct location
#
# usage: clone-pulumi-repo <repo name>
function clone-pulumi-repo() {
    local repoName=$1
    if [ ! -e "$(go env GOPATH)/src/github.com/pulumi/${repoName}" ]; then
        git clone "https://github.com/pulumi/${repoName}" "$(go env GOPATH)/src/github.com/pulumi/${repoName}"
        pushd "$(go env GOPATH)/src/github.com/pulumi/${repoName}" > /dev/null

        if git show-ref --verify --quiet "origin/${TRAVIS_PULL_REQUEST_BRANCH:-${TRAVIS_BRANCH}}"; then
            git checkout "origin/${TRAVIS_PULL_REQUEST_BRANCH:-${TRAVIS_BRANCH}}"
        fi

        popd > /dev/null
    else
        echo "repository '${repoName}' already cloned at $(go env GOPATH)/src/github.com/pulumi/${repoName}"
    fi
}
