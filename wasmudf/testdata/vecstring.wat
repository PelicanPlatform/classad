;; vecstring -- a vectorized UDF that returns a STRING state from the vector entry
;; point. The vector ABI carries no string storage in either direction, so a guest
;; claiming one has produced a value with nothing behind it; the host refuses it
;; rather than reading the payload as an offset into memory it never wrote.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $i i32) (local $rSt i32)
    (local.set $rSt (i32.load offset=8 (local.get $res)))
    (block $end
      (loop $next
        (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
        (i32.store8 (i32.add (local.get $rSt) (local.get $i)) (i32.const 5)) ;; vsString
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 1))
    (i32.const 0))
)
