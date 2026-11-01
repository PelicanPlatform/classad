;; vecredirect -- rewrites the result descriptor to point at a buffer it owns,
;; fills that buffer with legal-looking values, and writes nothing to the slots
;; the host allocated.
;;
;; The host must ignore the rewrite. The result descriptor is the host saying
;; where the guest should write, not the guest saying where the host should read:
;; following it would mean reading bytes the host never poisoned, so a guest that
;; wrote nothing could have an answer taken from any region of its own memory that
;; happened to hold legal state codes. Every element here must come back ERROR.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $i i32)
    ;; A buffer of our own: states at 1024, payloads at 2048.
    (block $end
      (loop $next
        (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
        (i32.store8 (i32.add (i32.const 1024) (local.get $i)) (i32.const 0))
        (i64.store (i32.add (i32.const 2048) (i32.shl (local.get $i) (i32.const 3)))
                   (i64.const 1234))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    ;; Point the descriptor at it and report success.
    (i32.store offset=8 (local.get $res) (i32.const 1024))
    (i32.store offset=12 (local.get $res) (i32.const 2048))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 1))
    (i32.const 0))
)
