#!/usr/bin/env python
"""Analyse one loopback capture: find the musical region, trim it, describe it.

Output: <dir>/analysis.json + <dir>/music.wav (trimmed, 48k stereo 16-bit)
        + <dir>/spectrogram.png (if matplotlib is available)
Usage: python scripts/analyze-capture.py --src .tmp/audio-capture/take2/music.wav --out DIR
"""
import argparse
import json
import os
import sys
import wave

import numpy as np

try:
    import librosa
except Exception as e:  # pragma: no cover
    print('librosa required:', e, file=sys.stderr)
    sys.exit(2)

NOTE_NAMES = ['C', 'C#', 'D', 'D#', 'E', 'F', 'F#', 'G', 'G#', 'A', 'A#', 'B']
# Krumhansl-Kessler key profiles
MAJ = np.array([6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88])
MIN = np.array([6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17])


def db(x):
    return float(20 * np.log10(max(float(x), 1e-9)))


def save_plot(mono, sr, path):
    import matplotlib
    matplotlib.use('Agg')
    import matplotlib.pyplot as plt
    from librosa import display
    fig, ax = plt.subplots(2, 1, figsize=(14, 6), sharex=True)
    D = librosa.amplitude_to_db(np.abs(librosa.stft(mono, n_fft=2048, hop_length=512)), ref=np.max)
    display.specshow(D, sr=sr, hop_length=512, x_axis='time', y_axis='log', ax=ax[0], cmap='magma')
    ax[0].set_title('spectrogram (log freq)')
    display.specshow(librosa.power_to_db(librosa.feature.melspectrogram(y=mono, sr=sr, n_mels=96), ref=np.max),
                     sr=sr, x_axis='time', y_axis='mel', ax=ax[1], cmap='magma')
    ax[1].set_title('mel')
    plt.tight_layout()
    plt.savefig(path, dpi=90)
    plt.close(fig)


def read_wav(path):
    try:
        with wave.open(path, 'rb') as w:
            n, ch, sw, sr = w.getnframes(), w.getnchannels(), w.getsampwidth(), w.getframerate()
            raw = w.readframes(n)
        if sw != 2:
            raise wave.Error('not 16-bit')
        a = np.frombuffer(raw, dtype='<i2').astype(np.float32) / 32768.0
        return a.reshape(-1, ch), sr
    except Exception:
        # float32 / 24-bit / flac: let librosa+soundfile deal with it (ACE emits float32 wav)
        y, sr = librosa.load(path, sr=None, mono=False)
        if y.ndim == 1:
            y = y[None, :]
        return y.T.astype(np.float32), sr


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--src', required=True)
    ap.add_argument('--out', required=True)
    ap.add_argument('--thresh', type=float, default=-50.0, help='dBFS gate for "music present"')
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)

    audio, sr = read_wav(a.src)
    mono = audio.mean(axis=1)
    dur = len(mono) / sr

    hop = int(sr * 0.05)
    rms = librosa.feature.rms(y=mono, frame_length=hop * 2, hop_length=hop)[0]
    times = np.arange(len(rms)) * 0.05
    rdb = 20 * np.log10(np.maximum(rms, 1e-9))

    above = rdb > a.thresh
    if not above.any():
        print('NOTHING above gate', file=sys.stderr)
        return 3
    # require 1 s of continuous signal to count as start/end
    need = 20
    idx = np.where(above)[0]
    start_i = end_i = None
    run = 0
    for i in idx:
        if run and i == prev + 1:
            run += 1
        else:
            run = 1
        prev = i
        if run >= need:
            start_i = i - need + 1
            break
    run = 0
    for i in range(len(above) - 1, -1, -1):
        if above[i]:
            run += 1
            if run >= need:
                end_i = i
                break
        else:
            run = 0
    if start_i is None or end_i is None:
        start_i, end_i = int(idx[0]), int(idx[-1])

    t0, t1 = max(0.0, times[start_i] - 0.5), min(dur, times[end_i] + 1.0)
    s0, s1 = int(t0 * sr), int(t1 * sr)
    clip = audio[s0:s1]
    mono_c = clip.mean(axis=1)

    # write trimmed wav
    p = os.path.join(a.out, 'music.wav')
    with wave.open(p, 'wb') as w:
        w.setnchannels(clip.shape[1])
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes((np.clip(clip, -1, 1) * 32767).astype('<i2').tobytes())

    res = {'src': os.path.abspath(a.src), 'capture_duration_s': round(dur, 2),
           'music_start_s': round(t0, 2), 'music_end_s': round(t1, 2),
           'music_duration_s': round(t1 - t0, 2),
           'peak_dbfs': round(db(np.max(np.abs(clip))), 1),
           'rms_dbfs': round(db(np.sqrt(np.mean(clip ** 2))), 1),
           'gate_dbfs': a.thresh}

    # per-2s loudness profile (structure hint)
    prof = []
    step = 2.0
    for t in np.arange(0, t1 - t0, step):
        seg = mono_c[int(t * sr):int((t + step) * sr)]
        if len(seg) < sr * 0.2:
            continue
        prof.append([round(t, 1), round(db(np.sqrt(np.mean(seg ** 2))), 1)])
    res['loudness_2s'] = prof

    # tempo
    onset_env = librosa.onset.onset_strength(y=mono_c, sr=sr, hop_length=512)
    tempo = librosa.feature.tempo(onset_envelope=onset_env, sr=sr, hop_length=512)
    res['tempo_bpm'] = round(float(np.atleast_1d(tempo)[0]), 1)
    try:
        beats = librosa.beat.beat_track(onset_envelope=onset_env, sr=sr, hop_length=512)[1]
        bt = librosa.frames_to_time(beats, sr=sr, hop_length=512)
        ibi = np.diff(bt)
        if len(ibi) > 3:
            res['beat_interval_median_s'] = round(float(np.median(ibi)), 3)
            res['beat_interval_std_s'] = round(float(np.std(ibi)), 3)
            res['n_beats'] = int(len(beats))
    except Exception as e:
        res['beat_error'] = str(e)

    # key
    chroma = librosa.feature.chroma_cqt(y=mono_c, sr=sr)
    cv = chroma.mean(axis=1)
    best = None
    for i in range(12):
        for mode, prof_k in (('major', MAJ), ('minor', MIN)):
            r = np.corrcoef(np.roll(prof_k, i), cv)[0, 1]
            if best is None or r > best[0]:
                best = (r, f'{NOTE_NAMES[i]} {mode}', i, mode)
    res['key'] = best[1]
    res['key_corr'] = round(float(best[0]), 3)
    res['chroma_12'] = {NOTE_NAMES[i]: round(float(cv[i]), 3) for i in range(12)}

    # timbre / bands
    stft = np.abs(librosa.stft(mono_c, n_fft=2048, hop_length=512))
    freqs = librosa.fft_frequencies(sr=sr, n_fft=2048)
    total = stft.sum() + 1e-9
    def band(lo, hi):
        m = (freqs >= lo) & (freqs < hi)
        return float(stft[m].sum() / total)
    res['band_energy_ratio'] = {'sub_20_80': round(band(20, 80), 4), 'bass_80_250': round(band(80, 250), 4),
                                'lowmid_250_800': round(band(250, 800), 4), 'mid_800_2000': round(band(800, 2000), 4),
                                'himid_2000_6000': round(band(2000, 6000), 4), 'air_6000_16000': round(band(6000, 16000), 4)}
    res['centroid_hz'] = round(float(np.mean(librosa.feature.spectral_centroid(y=mono_c, sr=sr))), 1)
    res['rolloff85_hz'] = round(float(np.mean(librosa.feature.spectral_rolloff(y=mono_c, sr=sr, roll_percent=0.85))), 1)
    res['flatness'] = round(float(np.mean(librosa.feature.spectral_flatness(y=mono_c))), 4)
    res['zcr'] = round(float(np.mean(librosa.feature.zero_crossing_rate(mono_c))), 4)
    res['onset_rate_per_s'] = round(float(len(librosa.onset.onset_detect(onset_envelope=onset_env, sr=sr, hop_length=512)) / max(1e-6, t1 - t0)), 2)

    # harmonic/percussive balance
    try:
        H, P = librosa.effects.hpss(mono_c)
        res['harmonic_ratio'] = round(float(np.mean(H ** 2) / max(1e-12, np.mean(H ** 2) + np.mean(P ** 2))), 3)
    except Exception as e:
        res['hpss_error'] = str(e)

    # rough segmentation via chroma self-similarity
    try:
        bounds = librosa.segment.agglomerative(chroma, 8)
        bt = librosa.frames_to_time(bounds, sr=sr, hop_length=512)
        res['section_bounds_s'] = [round(float(x), 1) for x in bt]
        res['section_bounds_s'].append(round(t1 - t0, 1))
    except Exception as e:
        res['seg_error'] = str(e)

    with open(os.path.join(a.out, 'analysis.json'), 'w', encoding='utf-8') as f:
        json.dump(res, f, ensure_ascii=False, indent=1)

    # spectrogram picture (imports live inside a helper: a top-level `import librosa.display`
    # in main() would shadow the module-level `librosa` and break every earlier use)
    try:
        save_plot(mono_c, sr, os.path.join(a.out, 'spectrogram.png'))
    except Exception as e:
        res['plot_error'] = str(e)

    print(json.dumps({k: v for k, v in res.items() if k not in ('loudness_2s', 'chroma_12')},
                     ensure_ascii=False, indent=1))
    return 0


if __name__ == '__main__':
    sys.exit(main())
