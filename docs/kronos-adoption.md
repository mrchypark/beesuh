# Kronos 설계 차용 기록

Beesuh는 [Kronos Agent OS](https://github.com/spyrae/kronos-agent-os)의 코드를
복사하거나 의존하지 않는다. 2026-09-06에 공개 main 문서와 소스를 읽고 Go/Rhiza
runtime에 맞는 설계 원칙만 채용했다. 조사 commit SHA는 기록하지 않았으므로 추정하지
않는다. 현재 구현의 진실은 native.go, native_tools.go, runtime.go와 HTTP/CLI 코드다.

~~~text
direct HTTP · interactive CLI
                    │
                    ▼
      request idempotency → bounded admission → session lock
                    │
                    ▼
 Rhiza run + native journal → OpenAI-compatible model → static tools
                    │                                      │
                    └────────── final result ──────────────┘
~~~

workspace_id + session_key는 대화, workspace_id + request_id는 실행을 식별한다.
native backend는 성공한 최근 turn만 journal에서 context로 재구성한다.

## 채택 표

| Kronos subsystem | 상태 | Beesuh 선택 |
| --- | --- | --- |
| session/durable turn 분리 | 구현 | Rhiza sessions, runs, native journal을 분리한다. request를 running으로 저장한 뒤 실행하고 final result를 저장한다. |
| append journal | 구현 | user/assistant/tool message, model metadata, tool intent/result를 순서대로 저장한다. 성공한 complete turn만 context에 넣는다. |
| request idempotency | 구현 | workspace 내 request ID와 input hash를 비교해 replay 또는 conflict 처리한다. |
| session serialization/admission | 구현 | session lock과 bounded active/waiting queue를 사용한다. Kronos에는 이 일반 HTTP pool이 없다. |
| context budget | 구현 | whole historical turn 단위로 제거하며 active tool call/result pair를 자르지 않는다. 현재 byte budget이다. |
| progressive static skills | 구현 | 목록을 prompt에 넣고 load_skill(name)만 허용한다. regular .md만 읽고 skill은 capability가 아니다. |
| opt-in note memory | 구현 | 대화별 최대 64 note를 explicit save/recall/forget으로 제공한다. FTS/vector/자동 추출은 없다. |
| static tools | 구현 | 독립 실행에서는 선택적 memory 도구와 load_skill만 제공한다. skill은 외부 실행 권한을 부여하지 않는다. |
| untrusted tool/memory data | 구현 | skill 내용과 recall note를 data로 구분하도록 지시하고 크기를 제한한다. 실제 도구 allowlist와 argument schema는 코드에서 검증한다. |
| interruption handling | 구현 | unknown external effect는 uncertain으로 막고 restart가 자동 실행하지 않는다. -recover는 interrupted로 terminal 처리한다. |
| status API | 구현 | active, queued, blocked, backend를 /v1/status로 제공한다. |
| tool schemas | 구현 | tool argument schema를 검증한다. |
| audit hash chain/redacted view | 계획 | journal은 남지만 tamper-evident chain, retention policy, operator audit view는 없다. |
| typed policy file | 계획 | capability는 CLI/env operator 설정이다. typed policy, precedence report는 없다. |
| human approval pause/resume | 계획 | per-call approval은 없다. 추가 시 durable request와 exact tool/args binding이 필요하다. |
| deterministic scripted turn/cassette eval | 부분 구현 | 실제 Rhiza와 scripted httptest model/tool fixture로 재시작, 도구 순서, 멱등성, 격리, 제한, 불명확한 효과를 검증한다. 별도 cassette 파일 기반 평가 runner는 없다. |
| FTS facts | 계획 | note가 작아 bounded scan을 쓴다. 실측 한계가 생기면 Rhiza search/index를 검토한다. |
| automatic summary/vector/KG/sleep compute | 보류 | 개인 데이터 추출 품질, retention, user control이 확정된 뒤 검토한다. |
| cron/scheduler | 보류 | CLI와 direct API가 입력 경로다. durable lease/idempotency/delivery 계약 없이 schedule을 넣지 않는다. |
| general MCP/dynamic tools | 거절(현재) | 정적 도구보다 공격 표면이 넓다. 필요 tool이 확정되면 static allowlist부터 추가한다. |
| Telegram/Discord/dashboard/UI | 보류 | transport/UI보다 runtime correctness를 먼저 완성한다. |
| swarm/supervisor/domain verticals | 거절(현재) | multi-agent arbitration, 경쟁·금융·분석 automation은 개인 비서 core runtime이 아니다. |

## 지켜야 할 불변조건

1. 외부 효과가 시작되기 전에 intent를 durable하게 기록한다.
2. 효과의 확인이 사라지면 성공으로 추정하거나 자동 재실행하지 않는다.
3. 재실행 가능한 tool은 downstream이 caller-generated idempotency key를 보존·중복 제거해야 한다.
4. 같은 session에는 하나의 active turn만 둔다.
5. context 예산을 넘어도 assistant tool call과 tool result를 분리하거나 절단하지 않는다.
6. memory, skill, 외부 tool output으로 도구 capability 설정을 변경할 수 없다. prompt injection 방지를 모델의 지시 준수만으로 보장하지 않는다.
7. direct HTTP/CLI 입력은 같은 Run path를 사용한다. service mode는 identity와 authorization을 요구한다.

Kronos의 durable resume는 차용하지 않는다. Kronos execute_tool은 external tool 성공 뒤
effect record를 쓴다. 그 사이 process가 죽으면 실제 효과가 발생했지만 ledger에 없을
수 있다. 외부 adapter가 Beesuh idempotency key를 원자적으로 보존·중복 제거하기 전에는
uncertain과 operator reconciliation이 정확한 동작이다.

memory_forget은 active memories row만 삭제한다. 이전 native journal, tool result,
완료 run transcript에 이미 들어간 내용은 유지된다. retention/export/delete 정책을
구현하기 전에는 이를 개인정보 완전 삭제라고 설명하지 않는다.

## 의도적으로 없는 범위

자동 scheduler, general MCP, dynamic code/tool, remote skill registry, automatic memory
extraction/summarization/vector search, approval UI, streaming, dashboard, distributed
worker lease는 구현하지 않았다. 이들은 기능 수보다 failure·authority·retention 계약을
먼저 요구한다. 현재 비서는 direct conversation, durable session/run,
static tool의 좁은 경로를 완성하는 데 집중한다.

레거시 OpenCode backend는 호환 경로로 남아 있다. native backend는 Go에서 model API와
tool loop를 직접 운영하므로 OpenCode/Kronk CLI나 저장소에 의존하지 않는다.

## 조사 근거

- [Architecture](https://github.com/spyrae/kronos-agent-os/blob/main/docs/ARCHITECTURE.md): transport가 agent pipeline으로 수렴하고 swarm은 optional subsystem이다.
- [Session store](https://github.com/spyrae/kronos-agent-os/blob/main/kronos/session.py): sessions, active turns, journal, tool results, approvals, effects ledger 분리.
- [Agent pipeline](https://github.com/spyrae/kronos-agent-os/blob/main/kronos/graph.py): durable callback과 exact approval argument scope.
- [ReAct engine](https://github.com/spyrae/kronos-agent-os/blob/main/kronos/engine.py): tool timeout, output cap, side effect, untrusted framing, selective parallelism.
- [Memory](https://github.com/spyrae/kronos-agent-os/blob/main/docs/MEMORY.md), [context engine](https://github.com/spyrae/kronos-agent-os/blob/main/kronos/memory/context_engine.py): persisted history budget과 layered recall.
- [Skills](https://github.com/spyrae/kronos-agent-os/blob/main/docs/SKILLS.md): catalog → skill → reference disclosure와 skill/capability 분리.
- [Governance](https://github.com/spyrae/kronos-agent-os/blob/main/docs/GOVERNANCE.md), [policy](https://github.com/spyrae/kronos-agent-os/blob/main/kronos/policy.py): fail-closed policy와 capability/approval/budget/retention.
- [Security](https://github.com/spyrae/kronos-agent-os/blob/main/docs/SECURITY.md), [audit](https://github.com/spyrae/kronos-agent-os/blob/main/kronos/audit.py): external data boundary, safe defaults, redaction, hash-chain의 한계.
- [Evals](https://github.com/spyrae/kronos-agent-os/blob/main/docs/EVALS.md): scripted turn과 cassette replay의 역할 분리.
- [Scheduler](https://github.com/spyrae/kronos-agent-os/blob/main/kronos/cron/scheduler.py): one-process/per-job overlap guard만으로 distributed trigger correctness를 보장하지 않는 이유.
