#!/usr/bin/env node
/**
 * 导演台闭环：给 pipelines.json 增补三条固定流水线（纯数据，不改 Go）。
 *
 *   storyboard      一句梗概 → 分镜表 JSON（不烧显存，先审再拍）
 *   film_from_plan  贴/改分镜表 → 逐镜生成 → 装配（跳过 LLM 重规划）
 *   text            一句话 → LLM 出稿（文本模态补齐：台词/文案/扩写）
 *
 * 幂等：已存在的 id 跳过；保留原文件其余内容与顺序。
 * 用法：node .tmp/add-pipelines-director.cjs [path ...]   缺省同时改源与运行副本
 */
"use strict";
const fs = require("node:fs");
const path = require("node:path");

const ROOT = path.resolve(__dirname, "..");
const DEFAULT_TARGETS = [
  path.join(ROOT, "data", "pipelines.json"),
  path.join(ROOT, "运行", "data", "pipelines.json")
];

const H3_SHOTLIST_DATA = {
  plan: "", backend: "h3", width: 608, height: 352, steps: 12,
  turbo: true, audio_check: true, interp: false, consistency_check: true
};

const ADD = [
  {
    id: "storyboard",
    name: "分镜表（先审再拍）",
    summary: "一句梗概 → 分镜表：景别/角度/轴线/运镜/光比/声音/转场。只规划、不烧显存，审完再拍",
    output: "text",
    est_seconds: 90,
    fields: [
      { key: "prompt", label: "片子梗概", type: "textarea", required: true,
        hint: "写清场景、人物关系、情绪、声音；全片无对白就明确写出来",
        bind: [{ node: "start-1", path: "prompt" }] },
      { key: "seconds", label: "目标时长（秒）", type: "int", default: 30, min: 10, max: 120,
        bind: [{ node: "sb-1", path: "seconds" }] },
      { key: "max_shots", label: "镜头数上限", type: "int", default: 6, min: 2, max: 12,
        bind: [{ node: "sb-1", path: "max_shots" }] }
    ],
    nodes: [
      { id: "start-1", type: "start", data: { prompt: "" } },
      { id: "sb-1", type: "storyboard", data: { brief: "", seconds: 30, max_shots: 6 } },
      { id: "end-1", type: "end", data: {} }
    ],
    edges: [
      { id: "e1", source: "start-1", target: "sb-1" },
      { id: "e2", source: "sb-1", target: "end-1" }
    ]
  },
  {
    id: "film_from_plan",
    name: "按分镜表出片",
    summary: "贴入（或改过的）分镜表 JSON → 逐镜生成 → 按 cut/continue/dissolve 装配；不再让 LLM 重规划",
    output: "video",
    est_seconds: 2400,
    fields: [
      { key: "plan", label: "分镜表 JSON", type: "textarea", required: true,
        hint: "从「分镜表（先审再拍）」拿到的 JSON，可直接改 景别/运镜/时长/转场 再拍",
        bind: [{ node: "sl-1", path: "plan" }] },
      { key: "width", label: "宽", type: "int", default: 608, min: 256, max: 1280,
        bind: [{ node: "sl-1", path: "width" }] },
      { key: "height", label: "高", type: "int", default: 352, min: 256, max: 1280,
        bind: [{ node: "sl-1", path: "height" }] },
      { key: "steps", label: "步数", type: "int", default: 12, min: 4, max: 30,
        bind: [{ node: "sl-1", path: "steps" }] },
      { key: "audio_check", label: "音频抽检", type: "bool", default: true,
        hint: "每段查有没有人声乱语/静音缺口，不合格显式报错",
        bind: [{ node: "sl-1", path: "audio_check" }] },
      { key: "consistency_check", label: "一致性门", type: "bool", default: true,
        hint: "开机前审分镜表：continue 续接却跳轴/换光 直接拦下",
        bind: [{ node: "sl-1", path: "consistency_check" }] }
    ],
    nodes: [
      { id: "sl-1", type: "shotlist", data: Object.assign({}, H3_SHOTLIST_DATA) },
      { id: "end-1", type: "end", data: {} }
    ],
    edges: [{ id: "e1", source: "sl-1", target: "end-1" }]
  },
  {
    id: "text",
    name: "文本生成",
    summary: "一句话需求 → 本地 LLM 出稿：台词 / 文案 / 扩写 / 分镜文案；不出图不占显存",
    output: "text",
    est_seconds: 120,
    fields: [
      { key: "prompt", label: "要它写什么", type: "textarea", required: true,
        bind: [{ node: "start-1", path: "prompt" }] },
      { key: "instruction", label: "写作要求", type: "text",
        default: "用中文直接给成品，不要解释，不要罗列步骤。",
        bind: [{ node: "llm-1", path: "prompt" }] },
      { key: "model", label: "模型（留空用默认）", type: "text", default: "",
        bind: [{ node: "llm-1", path: "model" }] }
    ],
    nodes: [
      { id: "start-1", type: "start", data: { prompt: "" } },
      { id: "llm-1", type: "llm", data: { model: "", prompt: "", text: "" } },
      { id: "end-1", type: "end", data: {} }
    ],
    edges: [
      { id: "e1", source: "start-1", target: "llm-1" },
      { id: "e2", source: "llm-1", target: "end-1" }
    ]
  }
];

function patch(file) {
  if (!fs.existsSync(file)) return { file, status: "skip (missing)" };
  const raw = fs.readFileSync(file, "utf8");
  const doc = JSON.parse(raw);
  if (!Array.isArray(doc.pipelines)) return { file, status: "skip (no pipelines)" };
  const ids = new Set(doc.pipelines.map(p => p.id));
  const added = [];
  for (const p of ADD) {
    if (ids.has(p.id)) continue;
    doc.pipelines.push(JSON.parse(JSON.stringify(p)));
    ids.add(p.id);
    added.push(p.id);
  }
  if (!added.length) return { file, status: "noop (already present)", count: doc.pipelines.length };
  fs.writeFileSync(file, JSON.stringify(doc, null, 2) + "\n", "utf8");
  return { file, status: "patched", added, count: doc.pipelines.length };
}

const targets = process.argv.slice(2).length ? process.argv.slice(2) : DEFAULT_TARGETS;
for (const t of targets) console.log(JSON.stringify(patch(path.resolve(t))));
