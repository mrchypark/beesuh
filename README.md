# beesuh

Beesuh는 Go 개인 비서 런타임이다. 기본 엔진은 OpenAI 호환 Chat Completions
API를 직접 호출하므로 OpenCode나 Kronk 설치가 필요 없다. 사용자 대화와 Compos
입력은 같은 `(workspace_id, session_key)` 대화로 들어오며, Conductor는 제한된
agent tool이다.

```text
사용자 CLI / HTTP ─┐
Compos 데이터 ────┼─ Beesuh ─ OpenAI 호환 모델 API
                  │     ├─ Rhiza: 실행, journal, 선택 memory
                  │     └─ Conductor: allowlist tool
```

현재 범위는 임베딩 가능한 런타임과 운영자용 CLI/HTTP다. 사용자 로그인 서버,
실제 OAuth/Codex 계정 연동 및 Compos 전체 워크플로의 완료를 의미하지 않는다.
[통합 범위와 검증 한계](docs/local-integration-status.md)를 참고한다.

## 실행

Go 1.27+가 필요하다. Rhiza는 공개 버전 `v0.18.0`으로 고정한다. 모델 URL에는 OpenAI 호환 API의 `/v1` base URL을 넣는다. 클라이언트가
`/chat/completions`를 붙인다. 모델은 tool calling을 지원해야 도구를 사용할 수 있다.

```sh
export BEESUH_MODEL_URL='https://model.example/v1'
export BEESUH_MODEL='your-model-id'
export BEESUH_MODEL_API_KEY='your-api-key'

go run ./cmd/beesuh \
  -model-url "$BEESUH_MODEL_URL" -model "$BEESUH_MODEL" \
  -workspace personal -session chat-1 -request-id first \
  -message '안녕, 오늘 할 일을 정리해 줘.'
```

API key가 없는 local endpoint라면 `BEESUH_MODEL_API_KEY`는 비워 둘 수 있다.
같은 workspace/session의 성공한 최근 turn은 다음 요청의 context로 복원한다.
`(workspace_id, request_id)`는 멱등 키다. 같은 입력 재전송은 저장 결과를
반환하고, 다른 입력으로 재사용하면 충돌한다.

### 이름 있는 provider

여러 계정·provider는 JSON registry로 선언한다. credential은 파일에 쓰지 않고
`api_key_env`로 환경 변수 이름만 참조한다. 예시는
[단일 계정 예제](examples/providers-go.json), [다중 계정 구성](examples/providers.json)과
[provider 문서](docs/providers.md)에 있다. 다중 사용자 서비스는 provider/connector
연결을 사용자·workspace에 등록하고, 요청마다 `connection_id`를 지정한다. 설정과
embedding 방식은 [connection 관리](docs/connections-design.md)를 참고한다.

다중 계정 구성은 등록한 모든 `api_key_env`의 환경 변수를 요구한다.

```sh
export OPENCODE_GO_WORK_API_KEY='set-outside-the-file'
go run ./cmd/beesuh -providers examples/providers-go.json -provider go-work \
  -interactive -workspace personal -session chat-1

go run ./cmd/beesuh -providers examples/providers-go.json -provider go-work \
  -workspace personal -session chat-1 -request-id first \
  -message '오늘 일을 정리해 줘.'
```

`-provider`를 생략하면 registry의 `default`를 사용한다. 같은 session의 history는
선택한 provider로 계속 보낸다. provider alias를 바꾸는 것은 의도적으로 같은 대화의
다음 turn을 다른 model에 보내는 동작이다. OpenCode Zen을 쓰려면 registry에
`kind: "opencode-zen"`, `api: "responses"`, model `gpt-5.3-codex`,
`api_key_env: "OPENCODE_ZEN_WORK_API_KEY"`처럼 추가하고 key는 환경에서 provision한다.
Zen model별 wire는 다르며 Gemini wire는 현재 구현하지 않았다.

### 직접 대화

```sh
go run ./cmd/beesuh \
  -model-url "$BEESUH_MODEL_URL" -model "$BEESUH_MODEL" \
  -interactive -workspace personal -session chat-1
```

`/quit`까지 같은 session을 유지한다. HTTP 서버는 private service bearer token이
필요하다.

```sh
export BEESUH_API_TOKEN='replace-with-private-service-token'
go run ./cmd/beesuh \
  -model-url "$BEESUH_MODEL_URL" -model "$BEESUH_MODEL" \
  -listen 127.0.0.1:8080

curl http://127.0.0.1:8080/v1/chat \
  -H "Authorization: Bearer $BEESUH_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"workspace_id":"personal","session_key":"chat-1","request_id":"second","message":"방금 말한 일을 세 항목으로 줄여 줘."}'
```

이 token은 신뢰된 운영자/서비스용이다. token 보유자는 이 런타임의 workspace와
session을 선택할 수 있다. 다중 사용자 서비스는 이 handler 대신 서비스가 검증한
`Principal`을 받는 `httpapi.NewAuthenticated`를 사용한다. service mode에서는
`connection_id`가 필수이며, 대화·run·history·memory는 사용자와 workspace로 분리된다.

### Compos trigger

Compos는 하나의 입력 trigger다. 기존 agent outbox의 **data object**
(`agent_definition`, `task_definition`, `input`, `subject_ref`, `_compos`)를
넣는다.

```sh
go run ./cmd/beesuh \
  -model-url "$BEESUH_MODEL_URL" -model "$BEESUH_MODEL" \
  -compos -session work-1 < examples/compos-input.json
```

HTTP에서는 `POST /v1/compos?session_key=work-1&provider=go-work`로 같은 JSON body를 보낸다.
provider query는 registry를 쓸 때만 필요하며 생략하면 default를 쓴다.
Adapter는 Compos input/output schema를 검증하고 workspace와 실행 request identity를
보존한다. Compos callback, 자동 결과 전달, push/pull은 아직 합의되지 않은 계약이라
구현하지 않았다.

### 조회 API

모두 bearer token이 필요하다.

- `GET /v1/status`: local active/queued/blocked 상태와 backend.
- `GET /v1/providers`: 이름, kind, API protocol, model, default만 반환한다. URL,
  credential 환경 변수명, API key, Codex auth directory는 반환하지 않는다.
- `GET /v1/history?workspace_id=...&session_key=...`: native backend의 최근
  200개 journal record. 입력과 모델·도구 결과가 포함될 수 있다.
- `GET /v1/runs/{request_id}?workspace_id=...`: 실행 결과 또는 `running` 상태.

### 서비스 사용자 연결

service mode의 `GET /v1/connections`는 현재 사용자가 소유한 연결만 보이며,
`DELETE /v1/connections/{connection_id}`는 해당 연결을 terminal `revoked`로 전환한다.
`GET /v1/providers`는 사용 가능한 provider마다 자신의 `connection_id`만 포함한다.
연결 등록은 public HTTP API가 아니다. 서비스의 신뢰된 provisioning 경로에서만
`Runtime.RegisterConnection`을 호출한다. service mode는 user/workspace authorization과
connection별 credential resolver를 함께 요구하며, credential은 Rhiza에 저장하지 않는다.
새 resolver는 서비스 secret store가 만든 connection-scoped `Credential.HTTPClient`와
nonsecret `AccountID`를 반환한다. OAuth token 갱신·저장은 그 client의 transport가 담당한다;
API key 입력은 기존 resolver 호환용으로만 지원한다.
상세 계약은
[connection 관리](docs/connections-design.md)에 있다.

## 선택 기능

`-memory`는 대화별 name/content note 저장을 켠다. 모델은 명시적 사용자 요청일 때만
`memory_save`를 호출하도록 지시받고 최대 64개 note를 저장한다.
`memory_forget`은 활성 note만 삭제한다. 과거 journal/transcript와 완료 run에는
내용이 남을 수 있으므로 전체 데이터 삭제 기능이 아니다.

`-skills-dir`는 operator가 설치한 regular `name.md` 파일(최대 32개, 각 8 KiB)을
정적으로 읽는다. 모델은 `load_skill`로 내용을 읽을 수 있지만 skill은 권한을
부여하지 않는다.

Conductor는 명시적으로 켠다.

```sh
export CONDUCTOR_URL='http://127.0.0.1:3000'
export CONDUCTOR_ALLOWED_EVENTS='echo'
export CONDUCTOR_EVENT_BINDINGS='{"echo":{"handler_key":"payload.echo","handler_version":"1","operation_key":"payload.echo"}}'
# deployment gateway가 요구할 때만 CONDUCTOR_TOKEN을 설정한다.
go run ./cmd/beesuh \
  -model-url "$BEESUH_MODEL_URL" -model "$BEESUH_MODEL" \
  -conductor-tools -workspace personal -session chat-1 -request-id workflow-1 \
  -message 'echo handler를 실행해 줘.'
```

허용 도구는 `conductor_emit`과 `conductor_get_run`뿐이다. event allowlist는
operator가 `CONDUCTOR_ALLOWED_EVENTS`로 주고, 각 이름의 등록 handler/version/operation을
`CONDUCTOR_EVENT_BINDINGS`로 명시한다. Conductor는 개별 handler execution을 큐에 넣으며
workflow 전체를 실행하지 않는다. 응답의 `execution_id`를 보존하여 `conductor_get_run`의
`run_id`로 조회한다(실제 endpoint는 `/v1/executions/{id}`). `_compos` linkage는 runtime이
생성하고 모델이 준 `_compos`/`subject_ref`는 거절한다. 실행 완료에는 Conductor worker가 필요하다. 현재 native backend에는 tool별
매 호출 승인 화면이 없으므로 신뢰된 사용자·서비스 경로에서만 켜야 한다.

## 저장, 제한, 실패

Rhiza는 실행 상태, native session journal, 선택 memory를 저장한다. credentials와
모델 API key는 저장하지 않는다. 한 data directory는 한 프로세스만 사용할 수 있다.
같은 session은 직렬 실행되고, 전체 active slot/waiting queue는
`-max-concurrent`/`-max-waiting`으로 제한된다. 기본 native loop는 최대 12
model/tool turn, 64 KiB context, 2,048 output token이다.

native 입력은 12 KiB, JSON 인코딩된 user message는 24 KiB까지다. 답변은
16 KiB, journal record는 24 KiB까지 저장한다. 큰 도구 결과는 원문 대신
`output_omitted`와 digest를 기록한다. context에는 최근 성공한 8개 turn 중
예산에 맞는 전체 turn만 넣으며, 저장된 원본 journal은 삭제하지 않는다.

Conductor event처럼 외부 효과가 가능한 tool은 intent를 먼저 기록한다. 전송 확인을
못 하면 `uncertain`으로 끝내고 자동 재시도·자동 side-effect replay를 하지 않는다.
이 경우 해당 workspace의 새 외부 효과는 durable 기록이 남아 있는 동안 차단된다.
재시작·새 session·새 connection으로 우회할 수 없다. 조회와 명시적 복구는 계속 가능하다.
레거시 OpenCode에서 도구를 허용한 세션은 prompt 또는 응답 검증 실패 시에도
`uncertain`으로 차단한다. abort 성공은 이미 실행된 외부 효과가 없다는 증거가 아니다.
`-recover`는 미완료 실행을 `interrupted`로 기록하며 외부 효과를 되돌리지 않는다.
서버를 멈추고 같은 data directory와 도구/스킬 설정으로 실행한다. 먼저 journal과
Conductor 상태를 확인해야 하며, 복구 이후 새 request ID로 같은 작업을 보내면
외부 작업이 중복될 수 있다. 기존 OpenCode session을 native로 자동 이관하지 않는다.

SSE/streaming, 일반 MCP, 동적 tool, 자동 요약/vector memory, per-call approval, UI,
자동 scheduler는 아직 없다. 채택 범위는
[Kronos adoption record](docs/kronos-adoption.md)에 있다.

## 레거시 OpenCode backend

기존 OpenCode session wrapper는 `-backend opencode`로 선택할 수 있다. 이 경로에는
OpenCode server와 해당 server의 model 설정이 필요하며 native memory/journal history는
제공하지 않는다. 새 배포는 native backend를 사용한다.
`examples/opencode/tools/conductor.ts`는 operator 전용 예제이며, Conductor URL·allowlist·
handler bindings와 `CONDUCTOR_WORKSPACE_ID`를 OpenCode server 환경에 명시해야 한다.
이 예제를 사용자별 gateway나 인증 위임으로 사용하면 안 된다.

레거시 `conductor_emit`은 기본으로 비활성이다. 이 경로를 명시적으로 켜려면
`CONDUCTOR_LEGACY_EMIT_UNSAFE=1`을 설정해야 한다. Conductor가 event를 받았는데
acknowledgement가 사라지면, 이후 OpenCode message가 message id가 다른 새 key로
중복 external effect를 만들 수 있다. 이 레거시 경로에는 durable unresolved-effect
기록이 없다. consequential work에는 native tool boundary를 사용한다.

## 검증

```sh
go test -race ./...
go vet ./...
go build ./cmd/beesuh
```


현재 GoAuthy 공통 credential-profile 확장 연동 목표, Conductor 실측 결과와 Compos의
미지원 계약은 [로컬 통합 상태](docs/local-integration-status.md)에 구분해 기록했다.

GoAuthy에서 연결한 여러 Codex 계정을 선택해 사용하는 consumer 격리와 실험적 app-server
인증은 [Codex 프로파일](docs/codex-profiles.md)에 설명했다.
같은 사용자의 여러 API-key 계정을 선택하는 인증 client 주입과 TLS 검증은
[GoAuthy credential delivery](docs/goauthy-api-key-delivery.md)에 설명했다.
OAuth access-token의 명시적 주입과 refresh 버전 검증은
[OAuth delivery](docs/goauthy-oauth-delivery.md)를 따른다.

### 로컬 Rhiza 개발

동시 개발할 때만 로컬 workspace를 사용한다. 이 파일은 커밋하지 않는다.

```sh
go work init . ../../project/rhiza
```

릴리스 검증은 `GOWORK=off go test -race ./...`로 고정된 의존성을 사용한다.
