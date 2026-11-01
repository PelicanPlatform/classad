;; vecdeclinetrap -- the vector entry declines every record; the scalar entry
;; works for EVEN values and traps on odd ones.
;;
;; It exists for the one case the escape hatch has that the scalar path does not:
;; an instance dying partway through a batch. The records already escalated have
;; real answers and must keep them; the record that trapped and everything after
;; it must be ERROR, because the instance is gone and a replacement would be
;; computing with a guest whose memory state nobody knows.
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
    (if (i64.ne (i64.and (i64.load offset=8 (local.get $a)) (i64.const 1)) (i64.const 0))
      (then unreachable))
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.add (i64.load offset=8 (local.get $a)) (i64.const 100)))
    (i32.const 0))
)
