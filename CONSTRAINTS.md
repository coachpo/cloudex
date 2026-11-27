# 项目约束

本文件登记项目设计约束，由 Repo Craft 的 `constraint-manager` 技能维护。条目有 `Source` 时以出处为准，否则以本文件为准。`active` 表示有效，`disputed` 表示待定，`superseded` 和 `retired` 表示失效。新立或变更约束前，由用户确认条文。

## 约束

### C-001 Codex 容器可随时丢弃

- Status: active

Codex 容器必须可随时丢弃并重建。容器实例和容器内内容均不持久化，重建不依赖旧容器或跨代保留的容器内状态。

## 变更记录

- CR-001 · 2026-11-25 · add · C-001
  - 理由：未说明。
  - 登记时的差异：当时的 ADR 0003 仍规定保留 Workspace 的 home 卷及其中的 Codex 状态。
