# DHTTP
A fork of github.com/golang/go/src/net/http

- For http/1.1 and http/2.0
- Includes arbitrary header and pseudo header ordering
- UTLS parroting supported
- hopefully kept up to date with net/http

## API
### Drop in compatible (mostly)
```
import (
    http "github.com/dteh/dhttp"
)
```
### Changes
To allow for a parrot to be set, `Transport` has been modified to accept `ClientHelloSettings`

```
type ClientHelloSettings struct {
	HelloID  tls.ClientHelloID
	Override tls.ClientHelloSpec
}
```
Users should set their desired parrot as `HelloID`. If a custom spec is required, set `HelloID` to `utls.HelloCustom` and `Override` to the custom spec.


## Reproduce this repository
```
git clone git@github.com:golang/go ./dhttp
cd dhttp
git filter-repo \
    --path=src/net/http \
    --path=src/internal/cfg \
    --path=src/internal/diff \
    --path=src/internal/godebug \
    --path=src/internal/godebugs \
    --path=src/internal/nettrace \
    --path=src/internal/profile \
    --path=src/internal/platform \
    --path=src/internal/goarch \
    --path=src/internal/bisect \
    --path=src/internal/race \
    --path=src/internal/testenv \
    --path=src/internal/txtar \
    --path-rename="src/internal/:internal/" --path-rename="src/net/http/:"

go mod init
go mod tidy

git remote add origin git@github.com:dteh/dhttp
```

- Grep and replace lol

```
"(internal/.+?)"$ => "github.com/dteh/dhttp/$1"
"net/http(.*?)"$ => "github.com/dteh/dhttp$1"
"net/http"$ => http "github.com/dteh/dhttp"
```


### pulling upstream changes
```
rm -rf ./upstream/
git clone git@github.com:golang/go ./upstream
cd upstream
git filter-repo \
    --path=src/net/http \
    --path=src/internal/cfg \
    --path=src/internal/diff \
    --path=src/internal/godebug \
    --path=src/internal/godebugs \
    --path=src/internal/nettrace \
    --path=src/internal/profile \
    --path=src/internal/platform \
    --path=src/internal/goarch \
    --path=src/internal/bisect \
    --path=src/internal/race \
    --path=src/internal/testenv \
    --path=src/internal/txtar \
    --path-rename="src/internal/:internal/" --path-rename="src/net/http/:"
```

add the folder as a local remote
```
cd ..
git remote add upstream-local "$(pwd)/upstream"
git merge upstream-local/master
```