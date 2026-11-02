# Mock robots (ROS 2, Gazebo 없이)

관제 서버 개발용 가짜 로봇입니다. 맵, 장애물, 센서 없이 **관제 서버가 보는 것만** 흉내 냅니다.
위치, 배터리, 상태, 목표 이동, 도킹·충전, 고장, 통신 끊김이 여기에 해당합니다.
이동은 목표까지 직선으로 갑니다.

## 실행 (Windows, 명령 프롬프트 cmd)

```bat
cd C:\pixi_ws
pixi shell
call C:\pixi_ws\ros2-windows\local_setup.bat
python <이 폴더>\mock_fleet.py --count 3
```

| 옵션 | 기본값 | 설명 |
|---|---|---|
| `--count` | 3 | 로봇 수 (`robot_1`, `robot_2`, ...) |
| `--speed` | 0.8 | 이동 속도 m/s |
| `--spacing` | 2.0 | 도크 간격 m. 로봇 i는 (시작 x + i×간격, 0)에서 시작, 거기가 도크 |
| `--origin-x` | 0 | 첫 도크의 x (다른 mock 프로세스와 떨어뜨려 놓을 때) |
| `--fault-rate` | 0 | 로봇당 분당 무작위 고장 횟수 |
| `--drop-seconds` | 10 | `drop_comms` 호출 시 침묵하는 시간 |
| `--roam-rate`, `--roam-max` | 0, 4 | 로밍 끊김: 로봇당 분당 횟수, 가장 긴 끊김(초) |
| `--battery` | 없음 | 모든 로봇의 시작 배터리(%). 없으면 60~100% 무작위 |
| `--drain-per-m` | 0.05 | 이동 1 m당 배터리 감소(%) |
| `--charge-rate` | 1.0 | 도크에서 초당 충전(%) |
| `--seed` | 없음 | 시작 배터리 난수 고정 |
| `--floor` | 1 | 모든 로봇의 시작 층 |
| `--floors` | 없음 | 로봇별 시작 층, 쉼표로(`1,1,3`: robot_3만 3층). `--floor`보다 우선 |
| `--exit-fail-floor` | 없음 | 이 층에서 엘리베이터 하차가 실패한다(문 구역이 막힘: 문 구역까지 갔다가 칸으로 돌아와 `failed`) |
| `--exit-fail-count` | -1 | 로봇마다 그 층에서 실패할 하차 횟수(-1: 매번) |
| `--door-move-s` | 0 | 엘리베이터 탑승·하차 한 번이 적어도 이만큼 걸린다(서버 turn 시간 초과보다 길게 해서 느린 로봇 흉내) |

## 로봇당 인터페이스 (네임스페이스 = 로봇 이름)

| 종류 | 이름 | 타입 | 비고 |
|---|---|---|---|
| pub | `odom` | nav_msgs/Odometry | 10 Hz, best effort (센서 QoS) |
| pub | `battery_state` | sensor_msgs/BatteryState | 1 Hz |
| pub | `robot_state` | std_msgs/String (JSON) | 2 Hz, reliable + transient local (늦게 붙어도 마지막 상태 받음) |
| sub | `goal_pose` | geometry_msgs/PoseStamped | 목표 지점 |
| srv | `cancel` | std_srvs/Trigger | 이동 취소 |
| srv | `dock` | std_srvs/Trigger | 도크로 가서 충전, 100%가 되면 IDLE |
| srv | `reset_error` | std_srvs/Trigger | ERROR 해제 |
| srv | `drop_comms` | std_srvs/Trigger | 일정 시간 모든 발행 중단 (heartbeat 감지 테스트) |
| srv | `rescue` | std_srvs/Trigger | 사람이 차에서 꺼냄: 탔던 로비 자리(지금 층)로, `in_car: false` |
| sub | `elevator_move` | std_msgs/String (JSON) | 엘리베이터 동작(6단계, 어댑터가 보냄), 아래 |

TF `map → <name>/base_link`도 발행합니다(RViz 확인용).

상태(`mode`): `IDLE`, `MOVING`, `DOCKING`, `CHARGING`, `ERROR`

`robot_state` 예시:
```json
{"name": "robot_1", "mode": "MOVING", "x": 1.2, "y": 0.4, "yaw": 0.32, "frame": "map",
 "battery": 87.3, "goal": [3.0, 1.0, 0.0], "error": "", "floor": 1, "in_car": false,
 "move": null, "stamp": 1793500000.12}
```

## 층과 엘리베이터 (6단계)

- 로봇마다 층이 있고(`--floor`, `--floors`) `robot_state`의 `floor`로 알린다. 어댑터가 이것을 `pose`의 `floor`로 보낸다.
- 목표(`goal_pose`)는 언제나 **지금 층**의 점이다. `frame_id`가 다른 층을 가리키면(`floor_3`, `floor3`) 무시한다. 층은 엘리베이터로만 바뀐다.
  차 안에 있거나 엘리베이터 동작 중이면 목표·`cancel`·`dock`을 거절한다.
- `elevator_move`(어댑터가 [docs/ELEVATOR.md](../docs/ELEVATOR.md)의 `turn`을 받으면 보냄):

  | 예 | 뜻 |
  |---|---|
  | `{"id": 7, "op": "board", "path": [[11, 0], [12.6, 0]]}` | 로비 → 문 → 자기 칸. 끝나면 `in_car: true` |
  | `{"id": 8, "op": "floor", "floor": 3}` | 차가 3층에 섰다: 로봇의 층이 3이 된다(차 안일 때만) |
  | `{"id": 9, "op": "exit", "floor": 3, "path": [[11, 0], [10, 0]]}` | 칸 → 문 → 3층 로비. 끝나면 `in_car: false`, 층 3 |

  결과는 `robot_state`의 `move`: `{"id": 9, "op": "exit", "status": "running"|"done"|"failed", "detail": ""}`. 같은 `id`는 한 번만 한다.
  동작 중 `mode`는 `MOVING`. `--exit-fail-floor` 층의 하차는 문 쪽으로 60% 갔다가 칸으로 돌아와 `failed`("door zone blocked")로 끝난다.
- 동작 중 고장(ERROR, `--fault-rate`·배터리 0)이 나면 그 자리에 멈추고 동작은 `running`으로 남는다(문 구역에 있을 수 있으므로 끝났다고 하지 않음).
  `reset_error`가 로봇을 그 동작의 시작점으로 되돌리고 그때 `failed`("fault in the door zone: ...")를 알린다.
- `rescue`: 사람이 차에서 꺼냄. 진행 중인 동작은 `failed`("taken out by a human"), 위치는 탔던 로비 자리, 층은 그대로(차가 있는 층). 어댑터가 이것을 보고 `rescued` 이벤트를 보낸다.
  어댑터를 거쳐 `fleet/<robot>/cmd` `{"type": "rescue"}`로도 부를 수 있다(와이파이가 있을 때).
- 와이파이 끊김(차 안)은 로봇이 아니라 **어댑터**가 흉내 낸다(차 안 로봇의 `fleet/*` 발행을 멈춤). 로봇은 ROS 안에서 평소대로 발행한다.
- 도크는 시작 위치의 x, y다. 다른 층에서 `dock`하면 그 층의 같은 x, y로 간다(층 개념이 없는 단순화).

## 손으로 테스트하기 (다른 cmd 창, 같은 환경 설정 후)

```bat
ros2 topic list
ros2 topic echo /robot_1/robot_state
ros2 topic pub --once /robot_1/goal_pose geometry_msgs/msg/PoseStamped "{header: {frame_id: map}, pose: {position: {x: 3.0, y: 1.0}, orientation: {w: 1.0}}}"
ros2 service call /robot_1/dock std_srvs/srv/Trigger
ros2 service call /robot_1/cancel std_srvs/srv/Trigger
ros2 service call /robot_2/drop_comms std_srvs/srv/Trigger
ros2 service call /robot_2/reset_error std_srvs/srv/Trigger
```

## 시뮬레이션 규칙

- 배터리: 켜져 있으면 초당 0.002%, 이동하면 m당 0.05%(`--drain-per-m`) 감소. 도크에서 초당 1%(`--charge-rate`) 충전.
- `dock`: 하던 이동을 멈추고 `DOCKING`으로 자기 도크(시작 위치)까지 간 뒤 `CHARGING`, 100%가 되면 `IDLE`.
- 이동 중 배터리 0% → `ERROR (battery_empty)`. ERROR 중에는 목표를 무시합니다.
- `--fault-rate`를 주면 무작위로 `ERROR (simulated_fault)`가 납니다.
- 배터리 부족 시 충전 보내기(서버의 배터리 정책, 루트 README), 작업 배정, 통신 끊김 판정은 **관제 서버가 할 일**이라 로봇 쪽에는 넣지 않았습니다.

## 나중에 실제 Nav2로 바꿀 때

`goal_pose`(PoseStamped)와 `odom`은 Nav2와 같은 타입입니다. 관제 서버가 이 두 개와 `robot_state`만 쓰도록 만들어 두면,
WSL/Ubuntu에서 Nav2 로봇을 띄울 때 `robot_state`를 내는 작은 어댑터 노드만 추가하면 됩니다.
