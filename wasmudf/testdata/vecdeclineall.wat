;; vecdeclineall -- declines every record. A legal guest, and a pointless one:
;; it is the scalar path with a vector call in front of it. It exists so a test
;; can assert the escape hatch is correct at its limit, and so the Escalated
;; counter has something unambiguous to report.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $i i32) (local $w i32)
    (block $end
      (loop $next
        (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $w (i32.add (local.get $decl)
                               (i32.shl (i32.shr_u (local.get $i) (i32.const 6)) (i32.const 3))))
        (i64.store (local.get $w)
          (i64.or (i64.load (local.get $w))
                  (i64.shl (i64.const 1)
                           (i64.extend_i32_u (i32.and (local.get $i) (i32.const 63))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.add (i64.load offset=8 (local.get $a)) (i64.const 100)))
    (i32.const 0))
)
