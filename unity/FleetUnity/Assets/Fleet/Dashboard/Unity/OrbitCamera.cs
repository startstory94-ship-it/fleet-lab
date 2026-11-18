// Mouse orbit / pan / zoom around a pivot that eases toward its target, so
// focusing a floor or following the car glides instead of jumping.
//   left drag: orbit   right or middle drag (or shift+left): pan   wheel: zoom
using UnityEngine;

namespace Fleet.Dashboard3D
{
    public sealed class OrbitCamera : MonoBehaviour
    {
        public Vector3 Target;          // where the pivot is heading
        public float Yaw = 30, Pitch = 18, Distance = 30;
        Vector3 pivot;
        bool placed;

        public void JumpTo(Vector3 target)
        {
            Target = pivot = target;
            placed = true;
        }

        void LateUpdate()
        {
            if (!placed) JumpTo(Target);
            bool shift = Input.GetKey(KeyCode.LeftShift) || Input.GetKey(KeyCode.RightShift);
            float dx = Input.GetAxisRaw("Mouse X"), dy = Input.GetAxisRaw("Mouse Y");
            if (Input.GetMouseButton(0) && !shift)
            {
                Yaw += dx * 4f;
                Pitch = Mathf.Clamp(Pitch - dy * 4f, 5f, 89f);
            }
            else if (Input.GetMouseButton(1) || Input.GetMouseButton(2) || (Input.GetMouseButton(0) && shift))
            {
                var rot = Quaternion.Euler(0, Yaw, 0);
                Target -= (rot * Vector3.right * dx + rot * Vector3.forward * dy) * Distance * 0.03f;
            }
            float wheel = Input.mouseScrollDelta.y;
            if (wheel != 0) Distance = Mathf.Clamp(Distance * Mathf.Pow(0.88f, wheel), 2f, 300f);
            if (Input.GetKey(KeyCode.LeftArrow)) Yaw += 60 * Time.unscaledDeltaTime;
            if (Input.GetKey(KeyCode.RightArrow)) Yaw -= 60 * Time.unscaledDeltaTime;
            if (Input.GetKey(KeyCode.UpArrow)) Pitch = Mathf.Min(89, Pitch + 40 * Time.unscaledDeltaTime);
            if (Input.GetKey(KeyCode.DownArrow)) Pitch = Mathf.Max(5, Pitch - 40 * Time.unscaledDeltaTime);

            pivot = Vector3.Lerp(pivot, Target, 1 - Mathf.Exp(-Time.unscaledDeltaTime * 5));
            var q = Quaternion.Euler(Pitch, Yaw, 0);
            transform.position = pivot - q * Vector3.forward * Distance;
            transform.rotation = q;
        }
    }
}
