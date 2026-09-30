# 서비스 소유 identity와 connection

Beesuh는 service mode에서 서비스 사용자를 대화 세션과 분리해 관리한다. 이 문서의
connection registry와 user/workspace isolation은 구현되어 있다. 브라우저 로그인 UI,
OAuth issuer, secret store와 refresh engine은 Beesuh가 제공하지 않으며 service credential
resolver가 소유한다.

## 경계

| 대상 | 소유자 | 저장되는 것 |
| --- | --- | --- |
| 서비스 로그인 | 호스트 서비스 | 브라우저·bearer session 검증 결과 |
| `Principal` | 호스트 서비스 | `user_id`, `workspace_id` |
| Beesuh connection | 서비스 | ID, owner, kind, configured target, opaque credential ref, state, generation |
| Beesuh 대화 | Beesuh | owner/workspace로 namespace된 session·run·journal·memory |
| credential | service credential resolver | 인증된 HTTP client 또는 account-specific CodexHome, stable nonsecret account ID |

`Principal`은 request JSON이나 `X-User-*` 같은 헤더에서 만들면 안 된다. 서비스가
자체 browser/bearer session을 검증한 뒤 `httpapi.NewAuthenticated` callback으로만
전달한다. `WithPrincipal`은 이미 검증된 background/queue 작업을 위한 trusted
integration API이며 authenticator가 아니다.

```go
runtime, err := beesuh.Open(ctx, beesuh.Options{
	DataDir: "/var/lib/beesuh/service",
	BackendID: "service-v1",
	RequireIdentity: true,
	Authorize: func(ctx context.Context, p beesuh.Principal) error {
		return service.UserIsActiveAndWorkspaceMember(ctx, p.UserID, p.WorkspaceID)
	},
	ResolveCredential: func(ctx context.Context, c beesuh.Connection) (beesuh.Credential, error) {
		// c.CredentialRef로 service의 cached auth client를 조회한다.
		// AccountID는 token/API key가 아닌 stable nonsecret provider account identity다.
		return service.ResolveBeesuhCredential(ctx, c)
	},
	Native: native,
	MaxConcurrent: 16,
	MaxWaiting: 128,
})
if err != nil { /* handle */ }

handler, err := httpapi.NewAuthenticated(runtime, func(r *http.Request) (beesuh.Principal, error) {
	return service.VerifyBrowserOrBearerSession(r)
})
if err != nil { /* handle */ }
```

`Authorize`는 실행 시작 전과 queued/background 작업의 tool 실행 전 다시 호출된다.
따라서 background path는 cookie를 보존할 필요가 없지만, 큐에 넣을 때 검증한
`Principal`로 `WithPrincipal`을 붙이고 실행 시점의 membership/activity를 재확인해야 한다.

service mode data directory는 identity mode로 Rhiza에 pin된다. 기존 operator-mode
directory를 service mode로 열거나 반대로 여는 것은 거절된다. 기존 데이터에는 소유권을
명시적으로 migration한 뒤 새 service directory를 사용해야 한다.

## Connection 등록과 사용

`Connection`은 secret을 포함하지 않는 service-owned 등록이다. target과 owner binding은
등록 뒤 바뀌지 않고, lifecycle state만 바뀐다. `CredentialRef`는 필수 opaque reference이며
JSON/API 응답에는 포함되지 않는다. state가 바뀔 때마다 generation이 증가한다.

```go
err := runtime.RegisterConnection(ctx, beesuh.Connection{
	ID: "conn_user_42_go_work",
	UserID: "user_42",
	WorkspaceID: "workspace_acme",
	Kind: "provider", // provider 또는 conductor
	TargetID: "go-work", // NativeOptions.Providers 또는 NativeOptions.Conductors alias
	CredentialRef: "secretref:user_42:opencode_go_work",
	State: "ready", // ready, needs_login, revoked
})
```

`ready` 등록도 서비스 resolver의 계정·실행 자격 증명 확인을 통과해야 한다. 확인 전 연결은
`needs_login`으로 등록한다. 이는 trusted admin/provisioning path만 호출한다. public HTTP는 creation endpoint를
제공하지 않는다. owner가 `DELETE /v1/connections/{connection_id}`를 호출하면 state는
`revoked`가 되고, 같은 ID는 다시 `ready`가 될 수 없다. 재인증은 fresh connection ID를
provision해야 한다. `needs_login`에서 `ready` 전환은 실제 인증을 완료한 trusted adapter만
수행한다. 런타임은 이 전환에서도 resolver가 인증된 계정을 반환하는지 확인하며,
소유권만으로 unresolved credential을 ready로 바꾸지는 않는다. migration 이전 service connection에 credential reference가 없으면 실행을
거절하며 fresh provisioning이 필요하다.

요청은 provider connection을 반드시 지정한다. `provider`를 함께 보내는 경우에는 그
alias가 connection의 target과 같아야 한다. `conductor_connection_id`는 선택이며,
Conductor tool을 쓸 때만 provider connection과 별도로 지정한다.

```http
POST /v1/chat
Content-Type: application/json

{
  "workspace_id": "workspace_acme",
  "session_key": "support-12",
  "request_id": "msg-018",
  "connection_id": "conn_user_42_go_work",
  "conductor_connection_id": "conn_user_42_workflow",
  "message": "배포 상태를 확인해 줘"
}
```

workspace가 authenticated principal과 다르거나, 다른 사용자의 connection을 가리키면
요청은 거절된다. Rhiza key는 stable user/workspace namespace를 사용하므로 같은
`session_key`와 `request_id`도 다른 사용자에게 충돌하거나 history를 노출하지 않는다.
`GET /v1/history`, `GET /v1/runs/{request_id}`, `GET /v1/connections`도 같은 principal 범위에서
동작한다. `GET /v1/status`는 service mode에서 operator-only이므로 노출하지 않는다.

`GET /v1/providers`는 ready provider connection만 반환하며 각 항목의
`connection_id`는 호출자의 것이다. endpoint, environment-variable name, key,
CodexHome은 반환하지 않는다.

Compos는 existing connector binding의 `connection_id`를 유지한다. service adapter는
authenticated principal의 workspace와 그 ID를 Beesuh request에 붙여야 하며, Compos
payload가 owner나 workspace를 결정하게 하면 안 된다.

## Connector와 provider의 credential 책임

`ResolveCredential(ctx, connection)`은 매 model/Conductor 외부 호출 직전에 owner가 확인된
ready connection으로 호출된다. callback은 `Credential{HTTPClient, AccountID}` 또는 CLI용
`Credential{CodexHome, AccountID}`를 반환한다. HTTP client는 이미 connection 범위로
인증되어 있으며, transport가 OAuth refresh와 새 token 저장을 소유한다. resolver는
connection별 call lock 안에서 호출된다. callback 안에서 같은 connection의
`SetConnectionState`를 호출하면 lock을 재진입하므로 호출하지 않는다.

`Credential.APIKey`는 기존 service resolver 호환용 입력이다. 런타임이 request 직전에
`auth.HeaderClient`로 인증 HTTP client로 바꾸고 원본 key를 model/Conductor 설정에 남기지
않는다. 새 service 구현은 raw key를 넘기기보다 secret store가 만든 `HTTPClient`를 넘긴다.
`CodexHome`은 Codex CLI 연결에서만 가능하며 HTTP client/API key와 함께 줄 수 없다.
Rhiza에는 raw key, access token, refresh token, API key hash를 저장하지 않으며, secret
store 구현은 서비스가 선택한다.

OAuth 연결은 서비스가 credential reference와 account version 기준으로 `auth.OAuthClient`를
한 번 만들고 재사용한다. `source`는 서비스 secret store에서 token을 읽고 필요하면 refresh하며,
`persist`는 바뀐 token을 **요청 전** 저장한다. client 내부에서는 이 순서가 직렬화되어
refresh-token rotation 경쟁을 막는다. 저장이 실패하면 해당 client가 pending token을 메모리에
유지하고, 다음 요청은 source 재호출보다 pending token 저장을 먼저 시도한다. 저장 전에는
외부 API를 호출하지 않는다. `persist`는 동일 token의 저장 재시도에 대해 멱등이어야 하며,
CAS 응답 유실 뒤 이미 같은 token이 저장된 경우도 성공으로 처리해야 한다.
새 client를 매 실행마다 만들면 이 보호가 사라진다.
여러 process가 같은 credential reference를 쓴다면 secret store의 CAS/version 또는 service
lease로 process 간 refresh owner도 하나로 만들어야 한다. 이 repository는 그 shared secret
store나 lease를 제공하지 않는다.

```go
// service cache key: connection.CredentialRef + accountVersion
client, err := auth.OAuthClient(baseClient,
	func(ctx context.Context) (*oauth2.Token, error) {
		return secrets.TokenAndRefresh(ctx, connection.CredentialRef)
	},
	func(ctx context.Context, token *oauth2.Token) error {
		return secrets.StoreRotatedTokenCAS(ctx, connection.CredentialRef, token)
	},
)
credential := beesuh.Credential{HTTPClient: client, AccountID: accountID}
```

이 client는 request context로 token을 얻으므로, 최초 browser 요청의 context에 묶어 장기
보관하지 않는다. service cache는 connection 폐기·재인증·account version 변경 시 client를
제거해야 한다.

API key provider(OpenCode Go/Zen, Codex API)는 resolver가 `auth.HeaderClient`로 만든 client를
제공한다. `AccountID`는 API key나 그 hash가 아니라 provider가 보장하는 stable nonsecret
account identity여야 한다.
remote session은 connection ID, generation, AccountID에 묶인다. opaque provider state에는
process nonce도 적용한다. 같은 connection이라도 계정이 바뀌거나 runtime을 재시작하면 opaque state는 쓰지
않고 portable history를 다시 보낸다.

Codex CLI는 resolver가 account-specific explicit `CodexHome`을 제공해야 하며, CLI가 login
owner다. Beesuh는 inherited user auth나 auth JSON을 읽거나 복사하지 않는다. 실제 OAuth
connector는 해당 connector adapter가 refresh owner다.

service mode의 Conductor는 named connection과 `RequireScope: true`인 gateway access
credential을 필수로 요구한다. 이는 Slack/Google 등의 external connector OAuth token이
아니며, 실제 external grant는 service/gateway adapter가 connection ID로 해석한다.
Beesuh는 trusted principal에서만 다음 scope headers를 전송한다.

- `X-Beesuh-User-ID`
- `X-Beesuh-Workspace-ID`
- `X-Beesuh-Connection-ID`

Conductor gateway는 service bearer를 검증하고 event emit과 run lookup 모두에서 이 scope를
권한 검사해야 한다. gateway가 작업을 큐에 넣으면 실제 실행 시점에도 사용자와 연결 상태를
다시 조회해야 한다. 이 헤더 자체는 권한을 부여하지 않는다. state revoke는 admitted call이
끝날 때까지 기다리고, 이후 새 외부 호출을 막는다. 이미 gateway가 수락한 외부 효과는
되돌릴 수 없다. Conductor/Compos의 gateway scope contract는 이 repository 밖의 명시적인
배포 계약이며, 여기서 그 서버 구현을 변경하지는 않는다.

## 참고한 커넥터 설계

[Amp Labs connectors 적용 기준](amp-connectors-adoption.md)에 공급자 카탈로그, 인증된
클라이언트 주입, OAuth 갱신 저장, capability 경계를 정리했다. 현재 구현과 후속 OAuth
연동에서 추가할 부분을 구분하며, Conductor gateway 인증과 외부 SaaS 계정 인증도 분리한다.


### Conductor execution wire contract (2026-09-07)

`AllowedEvents`의 각 이름은 operator가 `Bindings`의 `HandlerKey`, `HandlerVersion`,
`OperationKey`에 명시적으로 연결한다. 일반 workflow 이름을 handler 이름으로 추측하지 않는다.
Native runtime은 요청의 workspace(서비스 모드에서는 인증된 Principal의 원래 workspace)와
안정적인 effect key를 `_compos.workspace_id` / `execution_request_id`로 주입한다.
`actor: {type: service, id: beesuh}`는 감사 메타데이터이며 위임 권한이 아니다.
모델 데이터의 `_compos`와 `subject_ref`는 거절한다. 응답 `execution_id`는 그대로 보존하며
`GetExecution` 및 기존 `GetRun`은 `/v1/executions/{id}`를 읽는다.

현재 Conductor 서버의 전역 service-token gate는 사용자별 실행/조회 권한을 검사하지 않는다.
따라서 그 서버를 `RequireScope: true` gateway라고 설정하면 안 된다. 이 옵션은 별도로
구현·검증된 gateway의 계약을 선언할 뿐 서버 권한 기능을 만들어 주지 않는다.
현재 직접 연동 범위는 신뢰된 operator의 non-SaaS handler fixture다. 사용자별 SaaS 실행,
취소, cross-client delegation, Compos callback은 이 adapter가 구현하지 않는다.
