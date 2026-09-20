# Sourced by a platform build driver; compilation only, no signing or private dependencies.
# shellcheck disable=SC2154
case "${1:?compile stage is required}" in
  helper)
cd "$root/backend"
helper="$build_root/embed/storagehelper/assets/$target/storage-helper"
mkdir -p "$(dirname "$helper")"
CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" -o "$helper.tmp" ./cmd/storage-helper
mv "$helper.tmp" "$helper"
;;
  application)
shasum -a 256 "$helper" | awk '{print $1}' > "$helper.sha256.tmp"
mv "$helper.sha256.tmp" "$helper.sha256"
overlay="$build_root/storage-helper-overlay.json"
product_overlay="$build_root/product-overlay.json"
python3 - "$overlay" "$root/backend/storagehelper/assets/$target/storage-helper" "$helper" "$root/backend/storagehelper/assets/$target/storage-helper.sha256" "$helper.sha256" <<'PY'
import json,os,sys
out,source_bin,built_bin,source_hash,built_hash=sys.argv[1:]
with open(out,"w",encoding="utf-8") as f: json.dump({"Replace":{os.path.abspath(source_bin):os.path.abspath(built_bin),os.path.abspath(source_hash):os.path.abspath(built_hash)}},f,separators=(",",":"))
PY
cd "$root/frontend"; npm ci; npm run build -- --outDir "$build_root/frontend/dist" --emptyOutDir
cd "$root/backend"
go run ./tools/webuiembed -dist "$build_root/frontend/dist" -output-root "$build_root" -virtual-root "$root/backend/api/webui_dist" \
  -base-overlay "$overlay" -helper-source "$root/backend/storagehelper/assets/$target/storage-helper" \
  -helper-built "$helper" -output "$product_overlay"
CGO_ENABLED=1 GOOS=darwin GOARCH="$arch" MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-11.0}" go build -mod=readonly -buildvcs=false -tags replicaro_embedded_ui -trimpath -overlay "$product_overlay" -o "$build_root/Replicaro-$target" .
;;
  *) echo "unknown compilation stage" >&2; return 2;;
esac
