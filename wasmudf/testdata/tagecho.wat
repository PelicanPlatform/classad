;; tagecho -- the "working function" fixture, and the one that proves every value
;; state survives a round trip through linear memory.
;;
;; One slot is 16 bytes: tag u32 @0, byte length u32 @4, payload u64 @8.
;; Tags: 0 undefined, 1 error, 2 boolean, 3 integer, 4 real, 5 string.
;;
;; It doubles a number, negates a boolean, echoes a string back by POINTER (which
;; is legal: the host's argument arena is ordinary guest memory, so returning the
;; pointer it was handed exercises the host's bounds check on a pointer it did not
;; itself choose to trust), and passes undefined/error straight through. Any other
;; tag is refused with a nonzero status rather than a guessed value.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32)
    (i32.const 1))

  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (local $tag i32)
    (local $len i32)
    (local $pay i64)

    ;; Arity is the registry's job, so a wrong argc here means the host is broken,
    ;; not the caller: refuse rather than read a slot that may not exist.
    (if (i32.ne (local.get $argc) (i32.const 1))
      (then (return (i32.const 1))))

    (local.set $tag (i32.load (local.get $args)))
    (local.set $len (i32.load offset=4 (local.get $args)))
    (local.set $pay (i64.load offset=8 (local.get $args)))

    ;; Default: same tag, empty payload. The branches below overwrite what they need,
    ;; which is what makes undefined and error cost no code at all.
    (i32.store (local.get $res) (local.get $tag))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 0))

    (block $done
      (if (i32.eq (local.get $tag) (i32.const 3))
        (then
          (i64.store offset=8 (local.get $res)
            (i64.mul (local.get $pay) (i64.const 2)))
          (br $done)))
      (if (i32.eq (local.get $tag) (i32.const 4))
        (then
          (i64.store offset=8 (local.get $res)
            (i64.reinterpret_f64
              (f64.mul (f64.reinterpret_i64 (local.get $pay)) (f64.const 2))))
          (br $done)))
      (if (i32.eq (local.get $tag) (i32.const 2))
        (then
          (i64.store offset=8 (local.get $res)
            (i64.xor (local.get $pay) (i64.const 1)))
          (br $done)))
      (if (i32.eq (local.get $tag) (i32.const 5))
        (then
          (i32.store offset=4 (local.get $res) (local.get $len))
          (i64.store offset=8 (local.get $res) (local.get $pay))
          (br $done)))
      (if (i32.le_u (local.get $tag) (i32.const 1))
        (then (br $done)))
      (return (i32.const 2))
    )
    (i32.const 0))
)
