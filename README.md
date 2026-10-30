# harbor-operator

既存の Harbor インスタンス上の Project を、Kubernetes のカスタムリソースとして
宣言的に管理する operator。

**Harbor 自体のデプロイは扱わない。** Harbor を Kubernetes 上に構築する
[goharbor/harbor-operator](https://github.com/goharbor/harbor-operator) とは
別のプロジェクト。想定しているのは、既にどこかで動いている Harbor に対して
project の作成・可視性・削除だけを GitOps に載せる使い方。

## できること

- `HarborProject` から Harbor の project を作成する
- 既に Harbor にある project を管理下に引き取る（`adoptExisting`）
- `public`（公開・非公開）を Harbor 側の変更も含めて揃え続ける
- Harbor 側で削除された project を作り直す
- リソース削除時に project を残すか消すかを選ぶ（`deletionPolicy`）

現時点でスコープ外: メンバー管理、ストレージクォータ、`auto_scan` などの
project metadata。いずれも後から optional フィールドとして足せる。

## 仕組み

リソースは 2 つ。

| Kind | スコープ | 役割 |
|---|---|---|
| `HarborConnection` | クラスタ | Harbor の場所と認証情報 |
| `HarborProject` | namespace | project 1 つ |

Harbor は watch できないので、`HarborProject` は `syncPeriod`（既定 10 分）ごとに
Harbor と突き合わせる。Harbor 側で手で変更した内容が戻るまで、最大でこの間隔かかる。

`HarborConnection` も同じ間隔で Harbor に接続を試み、資格情報が受け付けられたかを
自身の Ready condition に出す。`HarborProject` はこの結果を待たずに自分で接続するので、
接続の失敗はその接続を使う `HarborProject` の Ready condition にも出る。

## インストール

タグ（`v*`）を push すると、GitHub Actions が `ghcr.io/satoruh/harbor-operator:<タグ>` に
イメージを上げ、そのタグを埋め込んだ `install.yaml`（CRD と controller 一式）を
GitHub Release に添付する。

```sh
kubectl apply -f https://github.com/satoruh/harbor-operator/releases/download/<タグ>/install.yaml
```

GitOps で配る場合は、この `install.yaml` を配布用のリポジトリに置いて ArgoCD などから
適用する。イメージはタグで固定されているので、リリースを差し替えない限り中身は動かない。

手元のビルドから入れる場合:

```sh
make install                                        # CRD だけ
make deploy IMG=<registry>/harbor-operator:<tag>    # controller 本体
```

controller は `harbor-operator-system` namespace に入る。以降、認証情報の Secret は
**この namespace** に置く。

## 認証情報を用意する

### Harbor 側: System レベルのロボットアカウント

Harbor の Administration → Robot Accounts で作る。

- **System レベルであること。** Project レベルのロボットでは project を作成できない
- 次の権限が必要

  | 範囲 | 権限 |
  |---|---|
  | System | `Project: List`、`Project: Create` |
  | Project（すべての project を対象にする） | `Project: Read`、`Project: Update`、`Project: Delete` |

  `Create` だけでは足りない。ロボットは自分が作った project のメンバーにならないので、
  作成には成功しても、その project を一覧にも ID でも見られず、更新も削除もできない。
- 名前は `robot$<name>` の形式になる。`$` を含むので、シェルで扱うときは
  シングルクォートで囲む（`"..."` や裸だと `$name` が展開されて消える）

**OIDC 認証の Harbor で、OIDC ユーザーの CLI secret は使えない。** UI やブラウザ経由では
動くのに API では 401 になるため混乱しやすいが、ロボットアカウント以外に選択肢はない。

### Kubernetes 側: Secret

```sh
kubectl -n harbor-operator-system create secret generic harbor-credentials \
  --from-literal=username='robot$harbor-operator' \
  --from-literal=password='<token>'

kubectl -n harbor-operator-system label secret harbor-credentials \
  harbor.satoruh.org/credentials=true
```

**`harbor.satoruh.org/credentials=true` ラベルは必須。** 無いと controller は読み取りを
拒否し、`HarborProject` が `CredentialsInvalid` になる。

このラベルは Secret 所有者による明示的な opt-in で、次の 2 つと組で意味を持つ。

- `credentialsRef` に namespace を書く欄が無い。controller は自分の namespace しか読まない
- controller の RBAC は `harbor-operator-system` の Secret に対する `get` のみ

つまり `HarborConnection` を作れる人が、クラスタ内の任意の Secret を controller に
読ませることはできない。

## HarborConnection

```yaml
apiVersion: harbor.satoruh.org/v1alpha1
kind: HarborConnection
metadata:
  name: harbor
spec:
  baseURL: https://harbor.example.com
  credentialsRef:
    name: harbor-credentials
  allowedProjects:
    - namespace: team-a
      names: ["platform", "team-a-*"]
  syncPeriod: 10m
```

| フィールド | 既定 | 説明 |
|---|---|---|
| `baseURL` | — | Harbor のルート URL。末尾スラッシュと `/api/v2.0` は付けない |
| `credentialsRef.name` | — | operator の namespace にある Secret 名 |
| `credentialsRef.usernameKey` | `username` | ユーザー名を保持するキー |
| `credentialsRef.passwordKey` | `password` | トークンを保持するキー |
| `caBundleRef.name` | なし | PEM 形式の CA 証明書を持つ ConfigMap。operator の namespace |
| `caBundleRef.key` | `ca.crt` | 証明書を保持するキー |
| `insecureSkipVerify` | `false` | 証明書検証を無効化する。開発用 |
| `allowedProjects` | なし | どの namespace がどの project を管理してよいか。**無ければ誰も使えない** |
| `syncPeriod` | `10m` | Harbor と突き合わせる間隔。最短 30s |

**`allowedProjects` に載っていない `HarborProject` に対しては Harbor に何もしない。**
ロボットアカウントは Harbor のすべての project に届くので、この一覧が無いと
`HarborProject` を作れる人なら誰でも、他の namespace が管理する project を引き取り、
公開し、削除できてしまう。

| フィールド | 説明 |
|---|---|
| `namespace` | `HarborProject` のある namespace。`*` はすべての namespace |
| `names` | 管理してよい project 名。`*` は任意の文字列に一致する（`team-a-*`、`*`） |

クラスタの利用者が 1 人だけなら `namespace: "*"`、`names: ["*"]` で全体を許可できる。
束縛済みの `HarborProject` も対象で、一覧から外すと同期も削除も止まる。

**`baseURL` にはリダイレクトが起きない最終的な URL を書く。** `http://` を書くと
Harbor 側の 301 で `https://` に飛ばされ、認証情報が落ちて失敗する。

## HarborProject

```yaml
apiVersion: harbor.satoruh.org/v1alpha1
kind: HarborProject
metadata:
  name: platform
  namespace: team-a
spec:
  connectionRef:
    name: harbor
  projectName: platform
  public: false
  deletionPolicy: Orphan
```

| フィールド | 既定 | 説明 |
|---|---|---|
| `connectionRef.name` | — | 使う `HarborConnection`。**変更不可** |
| `projectName` | — | Harbor 上の project 名。小文字のみ。**変更不可** |
| `public` | 未設定 | 公開設定。**未設定なら管理せず、Harbor の値をそのまま残す** |
| `adoptExisting` | `false` | 同名 project が既にあるとき、それを引き取るか |
| `deletionPolicy` | `Orphan` | リソース削除時に Harbor の project をどうするか |

`connectionRef` と `projectName` を変更不可にしているのは、どちらも変更すると
リソースが別の project を掴むか、元の Harbor に project が取り残されるため。
どうしても変えたい場合はリソースを作り直す（後述）。

`deletionPolicy` の既定が `Orphan` なのは、上記のとおり作り直しが唯一の変更手段に
なるため。`Delete` が既定だと、移設のつもりの削除で実 project が消える。

`adoptExisting` が見られるのは、名前で見つけた project に束縛しようとするときだけ。
束縛後にこのフラグを消しても project は解放されない。ただし束縛していた project が
Harbor から消え、別の project が同じ名前で作られていた場合は、それも名前で見つけた
project として扱うので `adoptExisting` が要る。

## 状態を見る

```console
$ kubectl get harborprojects -A
NAMESPACE   NAME       PROJECT    ID   PUBLIC   READY   AGE
team-a      platform   platform   7    false    True    5m
```

接続そのものは `HarborConnection` で確認できる。

```console
$ kubectl get harborconnections
NAME     URL                          VERSION            READY   AGE
harbor   https://harbor.example.com   v2.15.2-a97e7b83   True    5m
```

`VERSION` は最後に接続できたときの値で、その後に接続できなくなっても残る。

うまくいかないときは Ready condition の `reason` を見る。

| reason | 意味 | 対処 |
|---|---|---|
| `Synced` | Harbor と一致している | — |
| `Connected` | Harbor に届き、資格情報が受け付けられた（`HarborConnection`） | — |
| `CredentialsRejected` | Harbor が資格情報を受け付けない（`HarborConnection`） | Secret の中身と、ロボットアカウントが有効かを確認する |
| `ConnectionUnavailable` | `HarborConnection` が無い、または client を組めない | 名前、`baseURL`、`caBundleRef` を確認する |
| `CredentialsInvalid` | Secret が無い、opt-in ラベルが無い、キーが足りない | 上記「認証情報を用意する」を確認する |
| `NotAllowed` | `HarborConnection` の `allowedProjects` がこの namespace と project 名の組を許可していない | `allowedProjects` に追加する |
| `ProjectInaccessible` | project はあるが、この資格情報では見えないか操作できない | ロボットアカウントの権限を上記の表に揃える |
| `AdoptionRefused` | 同名 project が既にある。束縛していた project が消え、別の project が同じ名前で作られていた場合も含む | 意図した project なら `adoptExisting: true` にする。Harbor 側で project を消した場合は次の同期で作成される |
| `InvalidProjectName` | Harbor が名前を拒否した | `projectName` を直す。リトライしても変わらない |
| `DeletionBlocked` | リポジトリが残っていて削除できない | 後述 |
| `InternalError` | Kubernetes API から Secret・ConfigMap・`HarborConnection` を読めなかった（無いのではなく、失敗か拒否） | controller の RBAC と API server の状態を確認する。backoff しながら自動でリトライする |
| `HarborError` | その他の Harbor 側の失敗 | backoff しながら自動でリトライする |

controller が Harbor に対して行ったことは、`HarborProject` の Event にも残る。

| reason | 種別 | 意味 |
|---|---|---|
| `ProjectCreated` | Normal | project を作成した |
| `ProjectRecreated` | Normal | 束縛していた project が Harbor から消えていたので作り直した |
| `ProjectAdopted` | Normal | Harbor に既にあった project に束縛した |
| `DeletionBlocked` | Warning | リポジトリが残っていて削除できない |
| `DeletionUnconfirmed` | Warning | 削除を確認できないままリソースを解放した。Harbor が 403 を返し、一覧にも project が無い。既に消えているか、この資格情報から見えないかのどちらか |

## よくある操作

### 既存 project を管理下に入れる

`adoptExisting: true` にして作成する。付けずに同名 project があると
`AdoptionRefused` で止まる。これは、意図しない project を掴むことを防ぐため
既定で拒否しているもの。

### 管理をやめる（project は残す）

`deletionPolicy: Orphan`（既定）のままリソースを削除する。Harbor には手が入らない。

### 別の Harbor へ移す

`connectionRef` は変更できないので、リソースを作り直す。

1. `deletionPolicy` が `Orphan` であることを確認する
2. `HarborProject` を削除する（Harbor の project は残る）
3. 移設先を指す `HarborConnection` を用意する
4. 新しい `connectionRef` で `HarborProject` を作り直す。移設先に同名 project が
   既にあるなら `adoptExisting: true` を付ける

### project を削除できない（`DeletionBlocked`）

Harbor はリポジトリを含む project の削除を拒否する。operator はこれを強制しない。
リポジトリを消すかどうかは人が決めることなので、リポジトリが残っている限り
リソースも finalizer を付けたまま残り、`DeletionBlocked` を報告し続ける。
リポジトリを削除すれば、次の同期で削除が完了する。

## 開発

```sh
make test     # 単体テストと envtest
make lint
make run      # 手元の kubeconfig で controller を動かす
```

`make run` では Pod の Downward API が無いため、認証情報を読む namespace は
kubeconfig の現在のコンテキストの namespace になる。

kind と開発用 Harbor を立てる場合:

```sh
./hack/dev-up.sh     # kind クラスタ + Harbor (http://127.0.0.1:30002, admin/Harbor12345)
./hack/dev-down.sh
```

e2e は専用の kind クラスタに Harbor を入れ、deploy した controller をロボットアカウントで
動かす。docker、kind、helm が必要。

```sh
make test-e2e        # クラスタの作成から破棄まで行う
```

Harbor client の実機テストは `HARBOR_URL` が設定されたときだけ動く。

```sh
HARBOR_URL=http://127.0.0.1:30002 \
HARBOR_USERNAME=admin \
HARBOR_PASSWORD=Harbor12345 \
go test -count=1 -run TestIntegration ./internal/harbor/
```

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
