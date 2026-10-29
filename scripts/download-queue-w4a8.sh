#!/bin/bash
# 接力下载：等 FL2VA W4A8 下载完（.part 消失、正式文件出现）→ 校验 sha256
# → 接力下载 video VAE int8_convrot（2.8GB）→ 校验。全部走 hf-mirror。
set -u
DM="D:/AI/Models/comfyui/diffusion_models"
VAEDIR="D:/AI/Models/comfyui/vae"
FL2VA="$DM/minimax_h3_fl2va_pruned_w4a8_mixed.safetensors"
VAE="$VAEDIR/minimax_h3_video_vae_int8_convrot.safetensors"

echo "[queue] waiting for FL2VA W4A8 to finish..."
for i in $(seq 1 720); do
  [ -f "$FL2VA" ] && break
  sleep 30
done
if [ ! -f "$FL2VA" ]; then echo "[queue] FL2VA not ready after 6h, exit 1"; exit 1; fi
echo "[queue] FL2VA file present, verifying sha256..."
cd "$DM"
echo "6e54649b33a54b9f0ac390d3e5b4e30c03bee9c8b5a1b294bba1fb4dd5213656  minimax_h3_fl2va_pruned_w4a8_mixed.safetensors" | sha256sum -c - || { echo "[queue] sha mismatch!"; exit 1; }
echo "[queue] FL2VA W4A8 verified OK"

if [ -f "$VAE" ]; then echo "[queue] VAE already present"; exit 0; fi
echo "[queue] downloading int8 VAE (2.8GB)..."
for i in $(seq 1 120); do
  curl.exe -L -C - -sS --retry 3 --retry-delay 3 -o "$VAE.part" \
    "https://hf-mirror.com/Comfy-Org/MiniMax-H3/resolve/main/vae/minimax_h3_video_vae_int8_convrot.safetensors" \
  && { mv -f "$VAE.part" "$VAE"; break; }
  sleep 5
done
cd "$VAEDIR"
echo "52a2c8c73583c86e4f41cdcce3a6ad0ea562987bc0bf3d60a0cef5f5c8e60c0e  minimax_h3_video_vae_int8_convrot.safetensors" | sha256sum -c - || { echo "[queue] VAE sha mismatch!"; exit 1; }
echo "[queue] VAE int8_convrot verified OK"
