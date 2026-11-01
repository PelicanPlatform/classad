;; vecbadbool -- a boolean element whose payload is neither 0 nor 1.
;;
;; The scalar ABI refuses that, so the vector ABI must too. A vectorized module
;; is two implementations of one function, and holding them to different contracts
;; is how they come to disagree: the engine reads a nonzero payload as TRUE while
;; the scalar path calls it malformed, so the same input would give two answers
;; depending on which entry point served it.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $i i32) (local $rSt i32) (local $rI i32)
    (local.set $rSt (i32.load offset=8 (local.get $res)))
    (local.set $rI (i32.load offset=12 (local.get $res)))
    (block $end
      (loop $next
        (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
        (i32.store8 (i32.add (local.get $rSt) (local.get $i)) (i32.const 2)) ;; boolean
        (i64.store (i32.add (local.get $rI) (i32.shl (local.get $i) (i32.const 3)))
                   (i64.const 5))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 2))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 1))
    (i32.const 0))
)
