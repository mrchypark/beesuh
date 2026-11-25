# 단일 노드 운영

이 문서는 Beesuh의 Rhiza 임베디드 런타임을 단일 프로세스로 운영하는 방법을 설명합니다.
로컬 CLI, Docker, Kubernetes의 영속 볼륨 운영을 다룹니다. 다중 writer와 HA 운영은 이 문서의 범위에 포함하지 않습니다.

## 런타임과 저장 경로

현재 소스는 Rhiza v0.27.4(커밋 `94e9ef3f29ddf52b3d543133bae3e3d8ba57558c`)을 사용합니다. Beesuh가 `rhiza.Open`을 호출하며,
`NodeID`는 `beesuh`, `PeerAddr`는 `127.0.0.1:0`입니다. 피어 주소는 루프백의
임시 포트이며 외부 서비스 주소로 사용하지 않습니다. `LocalArchive`와 `ObjStore`는
설정하지 않습니다. 기본 `Local:false`는 단일 멤버 임베디드 voter 구성이며,
LocalArchive 모드나 원격 아카이브 복구를 뜻하지 않습니다.

| 실행 방식 | Beesuh 데이터 루트 | Rhiza DataDir | 프로세스 잠금 |
| --- | --- | --- | --- |
| 로컬 CLI 기본값 | 현재 작업 디렉터리의 `.beesuh` | `.beesuh/rhiza` | `.beesuh/runtime.lock` |
| README Docker 예제 | `/data` | `/data/rhiza` | `/data/runtime.lock` |
| Kubernetes 예제 | `/data/beesuh` | `/data/beesuh/rhiza` | `/data/beesuh/runtime.lock` |

`-data-dir`는 Beesuh 데이터 루트입니다. 런타임은 그 아래 `rhiza`를 SDK의 DataDir로
전달합니다. 로컬 CLI를 재시작할 때는 같은 작업 디렉터리를 사용하거나
`-data-dir`에 고정된 절대 경로를 지정하세요.

근거: [runtime.go](../runtime.go), [CLI](../cmd/beesuh/main.go),
[go.mod](../go.mod), [README](../README.md).

## 단일 writer와 데이터 보존

같은 데이터 루트는 한 프로세스만 사용해야 합니다. Beesuh는 `runtime.lock`에
비차단 exclusive `flock`을 획득하며, 이미 사용 중이면 시작을 거부합니다.
잠금 파일을 지워서 사용 중인 디렉터리를 다시 열지 마세요. 컨테이너를 교체할 때도
기존 프로세스가 종료된 뒤 같은 영속 볼륨을 새 프로세스에 연결해야 합니다.

볼륨의 수명은 프로세스나 컨테이너의 수명과 분리해서 관리하세요. Docker 예제의
`beesuh-service-data:/data`는 named volume이며, 컨테이너를 교체해도 같은 볼륨을
계속 연결해야 기록을 유지할 수 있습니다. 볼륨을 삭제하거나 빈 볼륨으로 바꾸는 것은
재시작과 다릅니다. 대화와 실행 기록에는 사용자 입력과 모델·도구 응답이 포함되므로
볼륨과 백업의 접근 권한 및 보관 기간도 관리해야 합니다.

Docker 이미지는 Alpine 3.22를 기반으로 하며 UID/GID `10001`로 실행됩니다.
호스트 디렉터리를 연결한다면 이 사용자가 데이터 루트에 쓰고 접근할 수 있어야 합니다.
설정과 OAuth 클라이언트 비밀 파일에는 읽기 권한이 필요합니다.
근거: [Dockerfile](../Dockerfile), [README의 Docker 및 데이터 안내](../README.md).

## HTTP 서비스 설정

인증 설정은 [README의 HTTP 및 Docker 실행 예제](../README.md)를 기준으로 준비합니다.
`-listen`에는 전체 `-service-config`와 `-providers` 구성이 필요하며 backend는
`native`여야 합니다. 설정 필드와 사용자 연결 승인 절차는
[OAuth2/OIDC 서비스 설정](http-service-adapter.md)을 따르세요.
issuer, introspection, UserInfo, client ID, 별도 client secret 파일, audience,
required scopes 및 Goauthy provider 매핑을 준비해야 합니다.

Dockerfile의 기본 명령만으로 인증된 HTTP 서비스를 구성할 수는 없습니다.
설정과 비밀 파일을 연결하고 README처럼 두 설정 플래그를 전달하세요.
`BEESUH_API_TOKEN`이나 공용 모델 API key는 사용자 인증과 승인된 모델 연결을
대신하지 않습니다. 로컬 CLI의 모델 자격증명 구성은 HTTP 인증 구성과 별개입니다.

## 종료와 재시작

CLI는 SIGINT와 SIGTERM을 받아 요청 컨텍스트를 취소합니다. HTTP 서버에는
최대 15초의 `Shutdown` 시간을 부여하고, 실패하면 서버 연결을 닫습니다.
그 뒤 지연 호출한 `Runtime.Close`가 실행되며 DB를 닫은 다음 잠금 파일을 닫습니다.
런타임의 lifecycle lock을 사용하는 작업과 DB 닫기를 기다리는 시간에는 별도의
상한이 없습니다. 따라서 15초는 프로세스 전체 종료 제한이 아닙니다.

현재 소스의 CLI는 `Runtime.Close` 오류를 `run()`의 반환값에 반영하여
프로세스가 0이 아닌 종료 코드로 끝나도록 합니다. Kubernetes 예제는
`ghcr.io/mrchypark/beesuh:v0.4.0`을 지정합니다. 기존 게시 이미지 `v0.3.0`에는 이 오류 전파
수정과 새 `-storage-init` 옵션 및 Rhiza v0.21.1이 없습니다.

종료 유예 시간은 HTTP 종료와 진행 중인 작업 및 DB 닫기를 고려해 설정하세요.
Kubernetes 예제의 종료 유예 시간은 120초이며, 이 시간 안에 DB 닫기가 완료된다는
보장은 없습니다.
SIGKILL이나 전원 손실은 정상 종료 절차를 실행하지 못합니다.
근거: [CLI](../cmd/beesuh/main.go), [Runtime.Close](../runtime.go),
[Rhiza embedded lifecycle](https://github.com/mrchypark/rhiza/blob/66e2416b52e3d64aa86134b7b0c8b70c5ace88be/docs/embedded-lifecycle.md#L9-L31).

## 장애 복구와 볼륨 손실

프로세스 장애 후에는 같은 버전과 설정으로 보존된 동일 DataDir를 다시 연결합니다.
메인 프로세스가 `rhiza.Open`을 호출하고 SDK가 로컬 영속 상태를 이용해 DB 복구를
담당합니다. 이것이 진행 중이던 모든 애플리케이션 작업의 자동 완료를 뜻하지는 않습니다.
실행 중으로 남은 기록이나 외부 효과의 결과가 불확실하면
[실패 복구와 저장 정책](uncertainty-and-retention-policy.md)에 따라 확인하세요.
`-recover`는 외부 효과를 취소하거나 되돌리는 명령이 아닙니다.

현재 구성에는 원격 아카이브가 없습니다. 전체 데이터 볼륨이나 PVC를 잃으면
현재 구성만으로 기록을 복구할 수 없습니다. 별도로 보존한 유효한 백업이 없다면
빈 볼륨을 연결해도 이전 대화·실행 기록은 돌아오지 않습니다.
같은 DataDir를 보존한 SIGKILL 후 재열기와 Pod 교체로 전체 `emptyDir`가 사라지는
상황은 다릅니다. 후자는 미전송 마지막 배치뿐 아니라 모든 로컬 기록을 잃을 수
있습니다. Operator 없는 단일 노드는 가능하지만 현재 바이너리에 object archive
설정 경로가 연결되어 있지 않아 No-PVC object DR을 제공하지 않습니다.

PVC 스냅샷은 정상 종료 절차를 거쳐 프로세스가 완전히 종료된 뒤에만 생성하세요.
15초 경과나 Pod 삭제 요청만으로 종료 완료를 판단하지 마세요. 백업과 원본 볼륨은
복원 결과를 검증할 때까지 보존해야 합니다. 이 문서는 스냅샷 복원 시험의 성공을
주장하지 않습니다.

기존 저장 형식이나 아카이브를 자동으로 이전하는 절차는 제공하지 않습니다.
v0.23.3에서 만든 DataDir를 v0.27.4로 다시 여는 검사는 아래 범위를 다룹니다.
업그레이드하기 전에 README의 안내를 확인하고 기존 데이터,
아카이브와 이전 바이너리를 보존하세요. 여기서 말하는 자동 이전 미지원은 기존
Rhiza 저장 형식의 변환에 관한 것으로, 런타임의 애플리케이션 스키마 migration
호출과는 구분합니다. 기존 파일을 복사해 호환성이 확인되었다고 판단하지 마세요.
근거: [README의 기존 Rhiza 데이터 업그레이드 안내](../README.md).

## Kubernetes의 로컬 초기화와 재열기 검증

[Kubernetes 구성](../deploy/kubernetes/README.md)은 단일 replica StatefulSet과
10Gi `ReadWriteOncePod` PVC를 사용하며 삭제·축소 시 PVC를 `Retain`으로 보존합니다.
실제 데이터 보존에는 스토리지와 PV reclaim 정책도 적용됩니다. UID/GID와
`fsGroup`은 모두 `10001`이며 별도 Rhiza 서버나 Operator를 사용하지 않습니다.

메인과 init은 동일 이미지 `ghcr.io/mrchypark/beesuh:v0.4.0`과
`imagePullPolicy: IfNotPresent`를 사용합니다. 배포 전 해당 버전의 릴리스 CI와
이미지 게시 결과를 확인하세요.
기존 게시 이미지 `v0.3.0`으로는 새 init 명령을 실행할 수 없습니다.
배포 전에는 클러스터에 접속하지 않고 구성을 렌더링할 수 있습니다.

```sh
kubectl kustomize deploy/kubernetes
```

릴리스 워크플로는 버전 태그의 커밋을 검사한 뒤 해당 버전 태그로만 이미지를
게시하며 `latest`나 커밋 SHA 이미지 태그는 게시하지 않습니다. 이미지를 재정의할 때는
Kustomize `images`로 양쪽 이미지를 함께 변경하고 명시적인 버전이나 digest를 사용하세요.
인증 ConfigMap과 Secret, 명시적인 context 및 namespace 설정은
[배포 안내](../deploy/kubernetes/README.md)를 따르세요.

init은 shell 없이 `/usr/local/bin/beesuh -storage-init -data-dir /data/beesuh`를
실행합니다. 이 명령은 기존 SDK 설정과 `runtime.lock`을 사용해 `rhiza.Open` 후
`Close`하고 종료합니다. Open 또는 Close 오류는 실패로 반환되어 메인 시작을 막습니다.
모델·provider·인증 설정은 필요하지 않으며 HTTP listener를 시작하지 않습니다.
저장소 열기 경로가 디렉터리를 생성하므로 별도 mkdir init은 필요하지 않습니다.
볼륨 권한은 CSI와 `fsGroup`으로 준비하며 호환되지 않는 기존 권한은 운영자가 수정해야 합니다.

메인은 init과 같은 이미지, PVC, `/data/beesuh/rhiza` DataDir와 저장소 식별 설정으로
다시 엽니다. 이는 보존된 PVC의 **로컬 초기화와 재열기 검증**이며 로컬 WAL 복구가
일어날 수 있습니다. 근거는 [embedded lifecycle의 Open/Close 계약](https://github.com/mrchypark/rhiza/blob/66e2416b52e3d64aa86134b7b0c8b70c5ace88be/docs/embedded-lifecycle.md#L9-L31)입니다.
현재 구성은 기본 임베디드 단일 voter `Local:false`이며 아카이브가 없습니다.
새 Local 아카이브 복구 기능이나 볼륨 손실 복구를 구현했다는 뜻은 아닙니다.
로컬 CLI와 Docker에서는 메인 시작이 SDK를 열므로 별도 init이 필요하지 않습니다.
선택적으로 초기화 명령을 실행하려면 기존 writer를 완전히 중지하고 동일 데이터
루트를 지정하세요. `runtime.lock`을 삭제해 실행을 강제하지 마세요.

Rhiza SDK의 object-store 기능은 Beesuh 바이너리의 설정 기능과 구분해야 합니다.
이번 업그레이드는 아카이브 연결이나 PVC 없는 복구 기능을 추가하지 않습니다.
이번 브랜치 밖에 보존된 기존 DR WIP는 이번 변경에 포함하지 않으며,
BeforeAck/RPO0나 분산 fencing을 새 릴리스 조건으로 요구하지 않습니다.

단일 replica와 볼륨 접근 모드는 fencing이 아닙니다. Pod 강제 삭제만으로 이전
프로세스의 종료를 판단하지 마세요. 교체 writer를 시작하기 전에 이전 writer가
중지되었음을 확인해야 합니다. init 성공만으로 서비스 준비 완료를 판단하지 마세요.

## 검증 범위

이 문서는 현재 소스와 커밋이 고정된 Rhiza 공식 문서를 대조한 운영 안내입니다.
CI는 Go 코드 형식, vet, 일반 테스트와 race 테스트, CGo 없이 빌드하는
Darwin arm64 바이너리, linux/amd64 Docker 빌드와 스모크 테스트를 검사합니다.
별도 교차 버전 검사에서는 v0.23.3으로 실행·대화 기록·세션을 만든 프로세스를
SIGKILL로 종료합니다. 같은 DataDir를 v0.27.4로 열어 모델을 다시 호출하지 않고
결과를 재사용하는지, 복구한 대화 기록으로 새 실행을 이어갈 수 있는지 확인합니다.
이 검사는 기존 connection·cancellation·recovery-fence 레코드의 교차 버전 호환성과
다운그레이드를 다루지 않습니다. 후보 커밋의 검사 결과는 해당 PR에서 확인하세요.
상세 범위는 [v0.27.4 평가](rhiza-0.27.4-assessment.md)를 참고하세요.

Docker CI는 동일 볼륨을 사용하는 init과 메인 실행 및 TLS 인증을 검사합니다.
해당 버전의 실제 검사 결과는 릴리스 CI와 릴리스 노트에서 확인하세요.
Kubernetes 배포·PVC·CSI 권한과 부착·Pod 종료 및 보존된 PVC의 재열기 동작도 검증하지 못했습니다.
실제 외부 인증·provider 연결이나 볼륨 손실 복구도 검증하지 않았습니다.
현재 구성에는 아카이브가 없으므로 별도의 아카이브 기능 테스트는 범위에 포함하지 않습니다.
