# Install the newest version; GOPRIVATE lets go fetch the private repo over git.
install_latest:
    GOPRIVATE='github.com/vieitesss/*' go install github.com/vieitesss/recado/cmd/recado@latest
