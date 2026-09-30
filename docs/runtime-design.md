# Beesuh runtime — 초기 OpenCode 협의 기록

> **대체됨:** 이 문서는 OpenCode wrapper를 기본안으로 삼았던 초기 조사 기록이다.
> 현재 기본 backend, 구현 범위, Kronos 설계 차용은
> [README](../README.md)와 [Kronos adoption record](kronos-adoption.md)가 우선한다.
> 아래 Compos result delivery 계약과 OpenCode 호환 경로는 역사적 참고로 유지한다.

상태: 2026-09-06 초기 구현 완료. 현재 동작과 실행 방법은 [README](../README.md)를 따른다.
아래는 기존 계약 조사와 설계 논의 기록이며, 미정 항목을 구현 완료로 간주하지 않는다.

## 확정된 요구사항

- Go로 구현하며 첫 실행 엔진은 OpenCode를 감싼다.
- session과 pool을 관리한다.
- Compos는 입력 데이터의 원천이다. Conductor는 에이전트가 호출하는 tool이다.
- Compos는 트리거 중 하나다. 사용자 직접 대화 HTTP/CLI 경로도 제공한다.
- OpenCode 자체 session을 유지하고, Beesuh는 session ID를 기록해 재사용한다.
- 외부 입출력은 Compos의 기존 정의를 따르고, 없는 정의는 함께 논의한다.
- DB가 필요하면 `../../project/rhiza`의 Rhiza를 사용한다.

## 현재 목표의 구조

```text
사용자 대화 ─┐
Compos 데이터 ┴→ Beesuh (Go) → OpenCode session
                  │               │
                  │               └─ tool 호출 → Conductor → 도구 결과
                  └─ Rhiza: 논리 session ↔ OpenCode session ID, 실행 상태
```

Compos에서 데이터를 받는 전송 방식과 결과를 돌려주는 계약은 별도 협의한다.
Conductor를 Beesuh의 상위 스케줄러로 두지 않는다. Conductor tool은 MCP 또는
OpenCode custom tool로 연결할 수 있으며 실제 Conductor 인터페이스를 확인해 선택한다.

## 참고: 확인한 Compos의 기존 실행 계약

아래는 기존 코드의 증거다. 현재 목표의 Beesuh 입력·출력 계약으로 자동 채택하지 않는다.

Compos는 Agent revision으로 Work를 라우팅하면 `agent.requested` journal fact와
outbox 요청을 함께 기록한다. 다음은 실제 outbox 데이터의 구조다.

```text
agent_definition: name, instructions, execution_policy,
                  optional model_policy/context_policy/tools/permissions/memory_policy
task_definition: pinned Task spec (input_schema, output_schema 등)
input: Work context
subject_ref: Work context
_compos:
  workspace_id, execution_request_id, handler_key, handler_version,
  workflow_instance_id, task_id, actor
  optional timeout_seconds, retry_policy, workflow_step_id, workflow_route_id
```

현재 전송 대상은 Conductor의 `POST /v1/events`이며, envelope는
`{events: [{id, name, idempotency_key, data}]}`다. `name`은 handler key,
`idempotency_key`는 원래 command ID다. 전송 성공 뒤 outbox 상태를 갱신하므로
중복 배달이 가능하다. 이 API는 **Compos → Conductor** 경계이며,
새로운 **Compos → Beesuh** 입력 방식은 별도로 정해야 한다.

결과는 `POST /internal/v1/conductor/callbacks`로 전달하는 기존 계약이 있다.
Bearer callback token과 아래 필드가 필요하다.

```text
callback_id, workspace_id, execution_request_id, execution_id,
provider_key, provider_run_ref, handler_version,
runtime_status: succeeded | failed | cancelled
attempt_count: positive integer
occurred_at: RFC3339
result: JSON object for success
optional error_class, error_detail, task_id, workflow_instance_id,
         correlation_id, causation_id
```

`provider_key`와 `handler_version`은 요청의 pinned handler identity와 같아야 한다.
콜백의 `result`가 Kernel 내부 `agent.record_result`의 `output`으로 변환된다.
Compos가 결과 스키마를 검증하고 `agent.succeeded`/`agent.failed`, `work.submitted`,
completion을 반영한다. Beesuh가 Compos DB를 직접 수정하지 않는다.
같은 callback ID의 재전송은 내용이 같아야 하며, 다른 terminal 결과는 충돌한다.

근거 (Compos 저장소 기준):

- `porting-go/internal/kernel/work.go:232`: AgentDefinition validation
- `porting-go/internal/kernel/work.go:1004`: agent route/outbox payload
- `porting-go/internal/server/conductor.go:446`: event batch delivery
- `porting-go/internal/server/conductor.go:41`: callback fields and validation
- `porting-go/internal/server/conductor.go:215`: result → output conversion
- `porting-go/docs/specs/http-api-contract.md:1093`: callback contract

## 구현 제안 — 논의 후 확정

OpenCode의 headless HTTP API를 Go `net/http`로 감싼다. 설치된 CLI는
`1.18.25`이며 `opencode serve`를 지원한다. 구현 시 해당 버전의 OpenAPI로
session 생성, prompt, abort, structured output 필드를 검증한다.
문서: https://opencode.ai/docs/server/ 및 https://opencode.ai/docs/sdk/

| 항목 | 제안 | 결정할 내용 |
| --- | --- | --- |
| 입력 | Compos 데이터를 Beesuh가 수신 | push/pull 및 요청·결과 계약 |
| tool | OpenCode가 Conductor 호출 | MCP 또는 custom tool의 좁은 API |
| pool | 단일 OpenCode 서버 + 동시 실행 슬롯부터 시작 | 격리·처리량 필요 시 여러 서버로 확대 |
| session | 논리 대화 키에 OpenCode session ID 매핑·재사용 | 논리 대화 키와 workflow route 간 분리 |
| 직렬화 | 같은 session에는 한 실행만 허용 | 대기 제한·timeout에 대기 시간을 포함할지 |
| 저장 | Rhiza에 session 매핑, 요청 식별자, 결과·전송 상태 저장 | 재시작 복구 범위와 보존 기간 |
| 정책 | 지원 키를 명시적으로 정의하고 검증 | model/context/tools/permissions/memory의 OpenCode 매핑 |

OpenCode 대화 session ID와 Compos 실행 요청 ID는 별개다. 대화를 재사용해도
실행별 identity와 결과를 분리한다. 최초 생성 ID를 저장하고 후속 입력은 같은 ID로
전송한다. 매핑에는 session을 보유한 backend/storage identity와 directory 범위도
포함해야 한다. ID만으로 빈 저장소의 다른 서버에서 대화가 복원되지는 않는다.
OpenCode 대화 이력은 OpenCode에 두고 Rhiza에 중복 구현하지 않는다.
Rhiza 사용 요구는 Beesuh가 새로 만드는 저장 계층에 적용하며, OpenCode 내장 저장소를
교체하는 것은 이 wrapper 제안에 포함하지 않는다.
Workflow 병렬 route가 같은 대화를 공유할지는 별도로 결정해야 한다.
프로세스 분리는 파일·도구 권한의 보안 격리를 대신하지 않는다.

요청을 durable하게 받아야 하는 경계에서는 Rhiza 저장 성공 이후 접수 성공을
응답한다. 동일 요청 ID의 다른 payload는 충돌로 처리한다. 실행 완료 결과와
callback identity·시각을 저장하고, 전송 재시도에는 같은 내용을 사용한다.
실행 중 프로세스가 죽으면 OpenCode 상태를 대조해야 하며, 도구 부작용이 있을 수
있으므로 미확인 실행을 자동 재실행하지 않는다. Beesuh의 실행 재시도와 Conductor의
tool 실행 재시도는 다른 범위이며, 부작용 있는 tool 요청은 해당 도구의 멱등 계약을 따른다.

Compos가 검증하는 정책 object에는 아직 OpenCode용 키 의미가 없다.
특히 허용 도구, permission 응답, working directory의 신뢰 경계, model 선택은
임의 기본값으로 확정하지 않는다. 성공 output은 Task/route 계약에 맞는 JSON
object여야 하며, workflow route 전용 output schema를 어떤 경로로 확보할지도
연결 계약에서 확인한다. 취소 요청 API는 현재 확인한 outbox/callback 계약에 없다.

## 대체 오픈소스 조사

공식 문서·소스 기준이며 실행 벤치마크나 실제 연동 검증은 하지 않았다.

| 후보 | 세션과 Go 연동 | Conductor tool 연결 | 판단 |
| --- | --- | --- | --- |
| OpenCode | headless HTTP API, session ID로 후속 prompt | native MCP 또는 custom tool | 현재 wrapper 목표의 우선 후보 |
| Goose | headless CLI JSON 출력, 저장 session ID로 resume | native MCP | 도구 중심 범용 에이전트의 대안 |
| Pi | JSONL stdin/stdout RPC, JSONL session 파일·ID 재사용 | extension/custom tool, MCP는 별도 extension | 작은 엔진과 높은 커스터마이징이 필요할 때 |
| Google ADK Go | Go Runner + session.Service | Go function tool/MCP toolset | 실행 루프까지 Go 내부에 두고 싶을 때 |

ADK Go의 Rhiza 연동은 자동으로 제공되는 기능이 아니다. session.Service를 Rhiza에
맞춰 구현해야 한다. 조사한 각 프로젝트의 세션 기능을 사용해도 Beesuh의 입력 계약,
논리 session 매핑과 동시 실행 제한까지 없어지지는 않는다.

출처:

- [OpenCode server](https://opencode.ai/docs/server/), [MCP](https://opencode.ai/docs/mcp-servers/), [session storage](https://opencode.ai/docs/troubleshooting/#storage)
- [Goose headless tasks](https://goose-docs.ai/docs/guides/running-tasks/), [sessions](https://goose-docs.ai/docs/guides/sessions/session-management/), [CLI extensions](https://goose-docs.ai/docs/guides/goose-cli-commands/)
- [Pi session·extension 설명](https://github.com/earendil-works/pi/tree/main/packages/coding-agent), [RPC](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md)
- [ADK Go](https://github.com/google/adk-go), [session.Service](https://github.com/google/adk-go/blob/main/session/service.go), [MCP tools](https://adk.dev/tools-custom/mcp-tools/)

## 구현 후 검증할 동작

- Compos 요청 fixture의 identity 보존과 결과 콜백 호환성
- 같은 session의 직렬화, 서로 다른 session의 pool 한도 준수
- timeout/cancel 시 OpenCode abort 확인 후 실행 슬롯 반환
- 중복 요청 충돌, 재시작 후 session 매핑과 미전송 결과 복구
- OpenCode 오류·잘못된 structured output이 성공으로 보고되지 않음

초기 구현은 직접 대화 HTTP/CLI, Compos JSON 입력 adapter, OpenCode client,
Rhiza session/run 저장, 단일 backend 동시성 pool, Conductor custom tool을 포함한다.
Compos 결과 자동 전송, 다중 backend 분산 pool과 미정 정책 매핑은 포함하지 않는다.
