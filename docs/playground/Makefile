# Makefile for the googlesqlite Playground.
#
# Typical first run:
#   make deps    # install npm dependencies
#   make dev     # build the engine, then start the Vite dev server
#
# `make build` produces the static site under dist/.

# Repository root, relative to this directory.
ROOT   := ../..
PUBLIC := public
WASM   := $(PUBLIC)/googlesqlite.wasm

.PHONY: build build/wasm build/assets build/page deps dev preview clean

build: build/wasm build/assets build/page

# build/wasm compiles the js/wasm Playground entrypoint
# (cmd/googlesqlite, the `js && wasm` build) to public/googlesqlite.wasm
# and copies the matching Go wasm_exec.js loader next to it. Everything
# under public/ is a generated artifact and is gitignored.
build/wasm:
	mkdir -p $(PUBLIC)
	cd $(ROOT) && GOOS=js GOARCH=wasm go build -ldflags='-s -w' \
	  -o docs/playground/$(WASM) ./cmd/googlesqlite
	# wasm_exec.js in GOROOT is read-only (0444) and lives in the read-only
	# module cache; a plain cp inherits that mode, so the NEXT build can't
	# overwrite it (EACCES). install creates a writable copy, replacing any
	# stale read-only one.
	install -m 644 "$$(go env GOROOT)/lib/wasm/wasm_exec.js" $(PUBLIC)/wasm_exec.js

# build/assets derives the artifacts that depend on the engine binary:
# the packed timezone database, the content-hash build id, and the
# pre-compressed .gz copy the server delivers (mirroring a deploy that
# ships the compressed artifact). Run it after build/wasm.
build/assets:
	node scripts/pack-zoneinfo.mjs
	node scripts/build-id.mjs
	gzip -kf -6 $(WASM)
	@ls -lh $(WASM) $(WASM).gz | awk '{print "build/assets: " $$9 " (" $$5 ")"}'

# build/page bundles the static site into dist/. The deployed site
# fetches the gzip-compressed engine and decompresses it in the worker,
# so the uncompressed copy Vite mirrors from public/ is dead weight on
# the host (and large enough to trip GitHub Pages' per-file limit);
# drop it to keep the published site small.
build/page:
	npm run build
	rm -f dist/googlesqlite.wasm

# deps installs the npm dependency tree.
deps:
	npm install

# dev builds the engine and starts the Vite dev server. The page is
# served at http://localhost:5173/googlesqlite/.
#
# With VITE_ENGINE=server the in-browser wasm engine is not built or
# loaded: the UI talks to the native engine server instead (run
# `go run ./cmd/playground` from the repository root first; see
# cmd/playground/README.md).
ifeq ($(VITE_ENGINE),server)
dev:
	npm run dev
else
dev: build/wasm build/assets
	npm run dev
endif

# preview serves the production build from dist/.
preview:
	npm run preview

clean:
	rm -rf dist node_modules $(WASM) $(WASM).gz $(PUBLIC)/wasm_exec.js \
	  $(PUBLIC)/zoneinfo.json $(PUBLIC)/build-id.json
