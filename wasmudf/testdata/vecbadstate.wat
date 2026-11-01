;; vecbadstate -- writes a state code the engine does not have.
;;
;; 7 is past the last real state, so it is neither a value nor the poison. The
;; host validates every element it reads rather than only checking for the poison,
;; because a guest inventing a state would otherwise have its payload read as
;; whatever the engine happens to do with an unknown state byte.
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
        (i32.store8 (i32.add (local.get $rSt) (local.get $i)) (i32.const 7))
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
