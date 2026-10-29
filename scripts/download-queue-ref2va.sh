#!/bin/bash
# 接力第 2 段：等 int8 VAE 下载完 → 下载 Ref2VA W4A8（11.7GB，台词镜用）。
set -u
DM="D:/AI/Models/comfyui/diffusion_models"
VAEDIR="D:/AI/Models/comfyui/vae"
VAE="$VAEDIR/minimax_h3_video_vae_int8_convrot.safetensors"
R2V="$DM/minimax_h3_ref2va_pruned_w4a8_mixed.safetensors"

echo "[queue2] waiting for int8 VAE..."
for i in $(seq 1 480); do
  [ -f "$VAE" ] && break
  sleep 30
done
[ -f "$VAE" ] || { echo "[queue2] VAE not ready after 4h"; exit 1; }
[ -f "$R2V" ] && { echo "[queue2] ref2va already present"; exit 0; }
echo "[queue2] downloading Ref2VA W4A8 (11.7GB)..."
for i in $(seq 1 600); do
  curl.exe -L -C - -sS --retry 3 --retry-delay 3 -o "$R2V.part" \
    "https://hf-mirror.com/AX1Y2JP/MiniMax-H3-W4A8-ConvRot/resolve/main/minimax_h3_ref2va_pruned_w4a8_mixed.safetensors" \
  && { mv -f "$R2V.part" "$R2V"; echo "[queue2] ref2va download done"; exit 0; }
  sleep 5
done
echo "[queue2] ref2va download failed after retries"; exit 1
