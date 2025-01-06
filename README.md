# FreeLens K8s Proxy

This repository is a fork of [lens-k8s-proxy](https://github.com/lensapp/lens-k8s-proxy/tree/main).

More secure alternative to `kubectl proxy`.

## How to build

On Mac and Linux install tools using [mise](https://mise.jdx.dev/):

```sh
mise install
make download
make build
```

On Windows:

```powershell
winget install GoLang.Go
winget install goreleaser.goreleaser
./Makefile.ps1 download
./Makefile.ps1 build
```

## License

Licensed under the [MIT license](./LICENSE).
