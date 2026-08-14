# Sub2API Plus

OpenAI/Codex のアカウント運用、クォータ棚卸し、固定プロキシ経路、上流互換性に重点を置いた AI API ゲートウェイです。

[English](README.md) | [中文](README_CN.md) | 日本語

[![Release](https://img.shields.io/github/v/release/zcj-ui/sub2api-plus)](https://github.com/zcj-ui/sub2api-plus/releases)
[![Dev Build](https://github.com/zcj-ui/sub2api-plus/actions/workflows/dev-build.yml/badge.svg?branch=dev)](https://github.com/zcj-ui/sub2api-plus/actions/workflows/dev-build.yml)
[![License](https://img.shields.io/badge/license-LGPL--3.0--or--later-blue)](LICENSE)

> **テクニカルプレビュー：** `0.2.x` は開発、互換性試験、隔離された受入試験向けです。独立したセキュリティ監査、長時間負荷試験、災害復旧試験、プロダクション認定は完了していません。本番環境、実課金ユーザー、代替不能なデータ、高価値の認証情報には直接使用しないでください。実行前にリポジトリ全体の[使用・展開・リスク免責事項](DISCLAIMER.md)と[完全なリスク通知](docs/legal/admin-compliance.en.md)を確認してください。これらの成熟度通知は LGPL の権利を制限しません。

## 概要

Sub2API Plus は [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) を基に継続開発している派生版で、上流の公式リリースではありません。著作権と帰属情報は [NOTICE](NOTICE) を参照してください。

主な機能：

- OpenAI OAuth/API Key、Codex、Claude、Gemini/Antigravity、Grok の統合ゲートウェイ。
- `/backend-api/wham/usage` から `credits.balance` を取得し、`Credit / 25` を参考 USD として表示。
- 選択したアカウントのクレジット、リセット期間、プロキシ接続、ヘルス状態を一括棚卸し。
- OpenAI アカウントのリクエスト、クォータ照会、ヘルスチェック、WebSocket を同じ固定プロキシへ経路指定。
- OpenAI/Codex では、上流から明示的な 429 を 2 回受けた後にアカウントをクールダウン。
- Claude Code、compact、実ツール継続、Shadow/Spark を除外した Codex 合成ツール履歴。
- 少数の正常アカウントを容量内で優先利用するスケジューリング。
- OpenAI、Anthropic、Gemini の一般的なリバースプロキシ URL と要求形式に対応。

`proxy_id` を設定していない OpenAI OAuth/API Key アカウントは従来どおり直接接続します。プロキシを設定した後は、リクエスト、OAuth 更新、クォータ照会、棚卸し、ヘルスチェック、WebSocket が同じプロキシに固定され、削除済み、空 URL、ID 不一致の場合は直接接続へ切り替えず失敗します。

## クイックスタート

最初の `vX.Y.Z` タグをまだ公開していない場合は、下記のソースビルドを使用してください。`ghcr.io/zcj-ui/sub2api-plus:latest` は正式タグのワークフローが成功した後にのみ公開され、ソースだけを push しても作成されません。

最初の正式タグ公開後：

```bash
mkdir -p sub2api-plus && cd sub2api-plus
curl -sSL https://raw.githubusercontent.com/zcj-ui/sub2api-plus/main/deploy/docker-deploy.sh | bash
docker compose -f docker-compose.yml up -d
```

正式タグのリリースイメージは `ghcr.io/zcj-ui/sub2api-plus:latest` です。起動後に `http://HOST:8080` を開いてください。これは試験用テンプレートであり、隔離環境でも `POSTGRES_PASSWORD`、`JWT_SECRET`、`TOTP_ENCRYPTION_KEY`、管理者パスワードを固定の強い値に変更してください。

詳細は [Docker デプロイガイド](deploy/README.md) を参照してください。

## ソースビルド

必要環境：Go `1.26.5`、Node.js `20+`、pnpm `9`、PostgreSQL、Redis。

```bash
git clone https://github.com/zcj-ui/sub2api-plus.git
cd sub2api-plus
cd frontend && pnpm install --frozen-lockfile && pnpm build
cd ../backend && go run ./cmd/server
```

```bash
make build-dev
make build-release
```

## リリースチャネル

| チャネル | トリガー | 出力 |
|---|---|---|
| 開発版 | `dev` ブランチへ push | マルチプラットフォーム成果物、`ghcr.io/zcj-ui/sub2api-plus:dev` |
| 安定版 | `vX.Y.Z` タグを push | GitHub Release、SHA256、マルチアーキテクチャイメージ、`latest` |

パッケージ版の更新元は `zcj-ui/sub2api-plus` です。必要な場合は `SUB2API_UPDATE_REPO` で上書きできます。

## ドキュメント

- [使用・展開・リスク免責事項（中国語）](DISCLAIMER.md)
- [新機能ガイド（中国語）](docs/releases/NEW_FEATURES_GUIDE_2026-08-14_CN.md)
- [変更履歴（中国語）](docs/releases/CHANGELOG_CUSTOM_2026-08-14_CN.md)
- [リリースと上流同期（中国語）](docs/releases/REPOSITORY_RELEASE_GUIDE_CN.md)
- [Docker デプロイ](deploy/README.md)

## セキュリティ

環境ファイル、実行時設定、データベース、ログ、Token、アカウント書き出し、プロキシ認証情報をコミットしないでください。試験には合成または破棄可能なアカウントを使い、HTTPS、固定暗号鍵、強い管理者認証、最小権限のデータベースアカウントを使用します。

サンプル設定、Compose、リリース成果物、`latest` イメージは本番向けに強化された構成ではありません。クォータとヘルス結果は時点スナップショットであり、`Credit / 25` の USD 表示は請求書、決済記録、与信または自動課金の根拠ではありません。更新や一括操作の前に、隔離環境でバックアップ、復元、ロールバックを検証してください。

## ライセンスと帰属

Sub2API Plus は [GNU LGPL v3.0 or later](LICENSE) で配布されます。

- 上流：[Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api)
- 現在の配布元：[zcj-ui/sub2api-plus](https://github.com/zcj-ui/sub2api-plus)
- 帰属情報：[NOTICE](NOTICE)

バイナリまたはコンテナを配布する場合は、対応するソース、ビルドスクリプト、`LICENSE`、`NOTICE`、`DISCLAIMER.md` を同梱してください。免責事項は運用上の成熟度を示すものであり、ライセンス条件を追加するものではありません。
