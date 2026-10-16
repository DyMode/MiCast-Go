#!/bin/bash
# Build an audio-only static FFmpeg from the pinned, unmodified sources.
set -euo pipefail
project="$(cd -- "$(dirname -- "$0")/.." && pwd)"
tools="$project/.tools/audio-build"
arch="${1:-amd64}"
case "$arch" in
  amd64) target=x86_64-linux-musl; host=x86_64-linux-musl; ffarch=x86_64; target_os=linux; suffix= ;;
  arm64) target=aarch64-linux-musl; host=aarch64-linux-musl; ffarch=aarch64; target_os=linux; suffix= ;;
  windows-amd64) target=x86_64-windows-gnu; host=x86_64-w64-mingw32; ffarch=x86_64; target_os=mingw32; suffix=.exe ;;
  *) echo "Unsupported target: $arch" >&2; exit 1 ;;
esac
zig="${ZIG:-$tools/zig-x86_64-windows-0.14.1/zig.exe}"
make="${MAKE:-$tools/make/usr/bin/make.exe}"
jobs="${JOBS:-4}"
platform_flags=()
# FFmpeg's non-debug MinGW workaround passes GNU-ld-only options to lld.
# -g0 keeps the validation build lean without that workaround.
if [ "$arch" = windows-amd64 ]; then platform_flags+=(--enable-debug --enable-debug=0); fi
work="$tools/work-$arch"
prefix="$work/prefix"
mkdir -p "$work/bin" "$prefix" "$work/lame" "$work/ffmpeg" "$work/output"
export ZIG_GLOBAL_CACHE_DIR="$tools/zig-cache"
export SOURCE_DATE_EPOCH=1781913600
for tool in cc ar ranlib ld; do
  if [ "$tool" = cc ]; then
    printf '#!/bin/bash\nexec "%s" cc -target %s "$@"\n' "$zig" "$target" > "$work/bin/$tool"
  elif [ "$tool" = ld ]; then
    printf '#!/bin/bash\nexec "%s" ld.lld "$@"\n' "$zig" > "$work/bin/$tool"
  else
    printf '#!/bin/bash\nexec "%s" %s "$@"\n' "$zig" "$tool" > "$work/bin/$tool"
  fi
  chmod +x "$work/bin/$tool"
done
export PATH="$work/bin:$tools/make/usr/bin:$PATH"
export CC="$work/bin/cc" AR="$work/bin/ar" RANLIB="$work/bin/ranlib" LD="$work/bin/ld" MAKE="$make"
printf '#!/bin/bash\nexec "%s" cc -target x86_64-windows-gnu "$@"\n' "$zig" > "$work/bin/host-cc"
chmod +x "$work/bin/host-cc"
cd "$work/lame"
if [ ! -f Makefile ]; then
  "$tools/lame-3.100/configure" --host="$host" --build=x86_64-pc-msys \
    --prefix="$prefix" --disable-shared --enable-static --disable-frontend \
    --disable-decoder --disable-analyzer-hooks --disable-nasm CFLAGS='-Os -fno-ident'
fi
"$make" -j"$jobs"
"$make" install
cd "$work/ffmpeg"
if [ ! -f config.h ]; then
  "$tools/ffmpeg-7.1.5/configure" \
    --enable-cross-compile --target-os="$target_os" --host-os=mingw32 --arch="$ffarch" \
    --cc="$CC" --host-cc="$work/bin/host-cc" --ar="$AR" --ranlib="$RANLIB" --pkg-config=false \
    --disable-everything --disable-autodetect --disable-network \
    --disable-doc --disable-debug --disable-asm --disable-x86asm \
    --disable-shared --enable-static --enable-small --disable-gpl --disable-nonfree \
    --disable-avdevice --disable-swscale --disable-postproc \
    --disable-ffplay --disable-ffprobe --enable-ffmpeg --disable-stripping \
    --extra-cflags="-Os -I$prefix/include" --extra-ldflags="-static -L$prefix/lib" \
    --enable-libmp3lame --enable-protocol=file,pipe \
    --enable-demuxer=aac,flac,mp3,wav,mov,ogg,pcm_s16le \
    --enable-decoder=aac,aac_fixed,aac_latm,alac,flac,mp3,mp3float,opus,vorbis,pcm_s16le,pcm_s16be,pcm_s24le,pcm_s24be,pcm_s32le,pcm_s32be,pcm_u8,pcm_f32le,pcm_f32be,pcm_f64le,pcm_f64be \
    --enable-parser=aac,aac_latm,flac,mpegaudio,opus,vorbis \
    --enable-encoder=libmp3lame,flac,pcm_s16le \
    --enable-muxer=mp3,flac,wav,pcm_s16le \
    --enable-filter=abuffer,abuffersink,aresample,aformat,anull,atrim,asetpts,anullsrc \
    --enable-bsf=aac_adtstoasc,extract_extradata "${platform_flags[@]}"
fi
"$make" -j"$jobs" ffmpeg"$suffix"
cp ffmpeg"$suffix" "$work/output/ffmpeg$suffix"
cp config.h config_components.h ffbuild/config.mak ffbuild/config.log "$work/output/"
cp "$tools/ffmpeg-7.1.5/COPYING.LGPLv2.1" "$work/output/"
cp "$tools/lame-3.100/COPYING" "$work/output/LAME-COPYING"
printf 'Built %s\n' "$work/output/ffmpeg$suffix"
