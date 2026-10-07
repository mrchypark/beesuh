# AgentField / agent-harness 도입 평가

평가일: 2026-09-19. 공개 소스 정적 검토와 현재 Beesuh 코드 대조.
AgentField `428c659fdb346f809afffa997355b2ea21ceeedf`,
agent-harness `cd42566aab9e21903e96b4d59ab7e674f7fb415d` 기준.
설치·벤치마크·실제 계정 연동·배포·런타임 교체는 수행하지 않았다.

## Pro 독립 검토

`codex-request` 프로젝트에서 Latest / Pro 선택을 확인하고 자문을 받았다.
[자문 대화](https://chatgpt.com/g/g-p-69ecdc42175c819186cf485b225c0e46-codex-request/c/6aae7e91-4bb4-83ee-8941-f42ff2d18c36).
Pro의 최종 권고도 C(기존 runtime 유지 + 선택적 차용)다. 별도 Go provider SDK 평가와
전체 control plane 도입을 분리하라는 의견을 반영했다. Pro가 지적한 기본 Env 맵의
얕은 복사·공유 변경은 고정 SHA의 `runner.go:202–230`을 다시 읽어 확인했다.

추가 핵심 의견: 외부 효과 도구를 추가하는 것만으로 효과 권한이 독점되지 않는다.
외부 harness의 내장 bash/네트워크 도구가 Beesuh의 도구 경계를 우회할 수 없어야 한다.
프로세스 취소는 이미 발생한 원격 효과의 취소 증거가 아니고, process-local lock은
분산 중복 실행 방지의 증거가 아니다. 권고는 소스 분석이며 통합 테스트 통과 주장이 아니다.

## 현재 판단

Beesuh 실행 런타임을 유지하고 필요한 어댑터 설계만 차용한다.
AgentField 전체 control plane은 현재 도입하지 않는다. 분산 worker 수요가 실제로 생기면
읽기 전용 작업 하나로 별도 평가한다. Go harness SDK만 쓰는 선택은 전체 플랫폼 도입과
다르지만, 현재 상태로 Beesuh의 다중 계정·무재시도 계약을 대체하지는 못한다.

## 확인한 근거

| 항목 | 소스에서 확인한 사실 | Beesuh에 미치는 영향 |
|---|---|---|
| Go SDK 재시도 | `maxRetries()`는 0 이하를 기본 3으로 처리, 실행 루프는 최초 실행+최대 3회 재시도. schema 재시도도 0 이하이면 기본 2 | `uncertain` 외부 효과의 자동 재실행 금지와 일반 Runner 기본 계약이 다름 |
| 환경변수 | CLI 실행은 `os.Environ()`에 override를 병합 | 계정별 HOME/인증·환경의 격리를 별도로 보장해야 함 |
| 공유 Env 맵 | `mergeOptions`가 DefaultOptions를 얕게 복사. 기본 Env가 non-nil이면 override가 같은 맵에 기록됨 | Runner 공유 시 호출 간 환경값 잔류, 동시 호출 시 맵 경합 가능 |
| Go 세션 | Options에 ResumeSessionID는 있지만 검토한 Codex/OpenCode provider는 이를 참조하지 않음 | 반환 session ID만으로 이어서 실행 지원을 추정할 수 없음 |
| 이벤트 | Go harness Provider 계약은 `Execute(...) (*RawResult,error)` | 내부 stdout 처리가 있더라도 외부 소비자용 공통 실시간 event stream과는 별도 |
| 저장소 | control plane의 LocalStorage는 SQLite/PostgreSQL 초기화 | Rhiza 고정 요구와 직접 부합하지 않음. 별도 DB 운영 또는 저장 계층 변경이 필요 |
| 분리 사용 | `sdk/go`는 독립 Go module | control plane 없이 provider 수준의 한정 사용을 평가할 수 있음 |

근거:
[Options](https://github.com/Agent-Field/agentfield/blob/428c659fdb346f809afffa997355b2ea21ceeedf/sdk/go/harness/provider.go),
[Runner](https://github.com/Agent-Field/agentfield/blob/428c659fdb346f809afffa997355b2ea21ceeedf/sdk/go/harness/runner.go),
[CLI 환경](https://github.com/Agent-Field/agentfield/blob/428c659fdb346f809afffa997355b2ea21ceeedf/sdk/go/harness/cli.go),
[Codex](https://github.com/Agent-Field/agentfield/blob/428c659fdb346f809afffa997355b2ea21ceeedf/sdk/go/harness/codex.go),
[OpenCode](https://github.com/Agent-Field/agentfield/blob/428c659fdb346f809afffa997355b2ea21ceeedf/sdk/go/harness/opencode.go),
[저장소](https://github.com/Agent-Field/agentfield/blob/428c659fdb346f809afffa997355b2ea21ceeedf/control-plane/internal/storage/local_init.go).

Provider.Execute 직접 호출은 Runner 재시도를 건너뛸 수 있지만 provider 내부 재실행까지
없애지는 않는다. 예를 들어 Codex provider에는 schema 거절 뒤 다시 실행하는 경로가 있다.
이는 전체 프로젝트가 위험하다는 주장이 아니라 Beesuh 요구와 특정 Go 경로의 차이다.

## 유지할 책임과 제거 가능한 코드

GoAuthy는 사용자·외부 계정·동의·credential 권한 원본, Beesuh는 대화·Connection 선택과
실행 승인을 담당한다. 독립 실행의 입력은 CLI/HTTP이며, 외부 작업 실행 서비스는 없다. 재시도 권한과 작업의
최종 상태를 두 런타임이 동시에 소유하지 않아야 한다.

어댑터가 동등한 계약을 통과하면 기존 CLI 실행/종료·stdout 해석 코드 일부를 대체할 수 있다.
그러나 Connection 소유권, credential 주입, 계정별 세션 binding, Rhiza journal과
request replay, uncertain-effect recovery는 공통 CLI 라이브러리가 대신하지 않는다.
분산 플랫폼을 붙여도 이 코드가 자동으로 삭제되는 것은 아니다.

## agent-harness에서 차용할 부분

Rust 라이브러리를 추가하기보다 Go 경계 설계에 다음을 반영할 가치가 있다:
공통 실행 이벤트(text/tool/usage/session/terminal), 명시적 cancel handle,
provider별 capability/readiness 구분, provider session과 서비스 session의 분리.
모든 기능을 지원하는 척하지 말고 미지원 resume/streaming은 capability로 드러낸다.
전체 event enum을 복제하거나 일반화된 프레임워크를 새로 만들지는 않는다.

[Harness 계약](https://github.com/getlatentic/agent-harness/blob/cd42566aab9e21903e96b4d59ab7e674f7fb415d/crates/agent-harness/src/harness.rs),
[실행 이벤트](https://github.com/getlatentic/agent-harness/blob/cd42566aab9e21903e96b4d59ab7e674f7fb415d/crates/agent-harness/src/events.rs).

## 도입 판단을 뒤집을 최소 실험

실제 필요한 provider 하나를 대상으로 기존 어댑터와 후보 어댑터를 같은 계약 테스트로 비교한다.
먼저 합성 CLI/외부 HTTP만 쓰며 운영 credential이나 플랫폼 배포는 필요하지 않다.

1. 같은 provider의 두 계정을 동시에 실행해 HOME/env/token/session 교차 사용 0회.
2. timeout/reset/잘못된 schema 뒤 실제 실행 횟수 1회; 효과 발생 뒤 응답 유실은 uncertain.
   내장 도구의 Beesuh 도구 경계 우회를 막고, 새 tool-call ID를 생성해도 같은 효과를 재실행하지 않음.
3. 취소 시 자식 process group 종료, 최종 이벤트 한 번, 후속 도구 실행 없음.
4. 같은 계정의 session resume 성공과 계정 변경 시 이전 session 재사용 거절.
5. 중간 이벤트·usage·오류가 공통 계약으로 전달되며 비밀은 로그/journal에 없음.
6. 재시작·중복 요청에서 Rhiza 원본과 효과 ID가 일치하고 이중 큐/최종 상태 소유권 없음.
7. 위 조건을 만족하면서 기존 코드를 실제 삭제하거나 필요한 기능을 더 적은 운영 부담으로 제공.

기본 Runner의 무재시도 설정과 profile 격리가 해결되지 않으면 외부 효과가 있는 경로에는
진입하지 않는다. 분산 플랫폼 채택은 별도로 queue/lease/recovery 수요와 운영 비용이 확인될 때
검토한다. 높은 에이전트 수를 내세운 README만으로 성능·안전성 통과를 판단하지 않는다.
