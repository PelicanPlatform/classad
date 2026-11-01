;; colvecdeclineall -- sets every decline bit and writes nothing at all.
;;
;; The escape hatch at its worst, and the case a columnar result has to be allowed
;; to leave empty: when no record was handled there is no column, so demanding a
;; state byte describing one would turn a batch whose every record has a correct
;; scalar answer into ERROR.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 2))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $w i32) (local $words i32)
    (local.set $words (i32.shr_u (i32.add (local.get $n) (i32.const 63)) (i32.const 6)))
    (block $end (loop $next
      (br_if $end (i32.ge_u (local.get $w) (local.get $words)))
      (i64.store (i32.add (local.get $decl) (i32.shl (local.get $w) (i32.const 3)))
                 (i64.const -1))
      (local.set $w (i32.add (local.get $w) (i32.const 1)))
      (br $next)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.mul (i64.load offset=8 (local.get $a)) (i64.const 2)))
    (i32.const 0))
)
